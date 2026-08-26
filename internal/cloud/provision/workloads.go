package provision

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// Where each workload's material is mounted. The signer's path is the value of
// CA_DIR, and loadCA appends the filenames to it, so this string and the
// filenames in derivedSecrets have to agree.
const (
	mountAtlantisTLS = "/tls"
	mountSignerPKI   = "/ca-private"
)

// pgSecretName is the Secret CloudNativePG generates for the application user.
// Its `uri` key is a complete DSN, so nothing here composes one: a hand-built
// connection string is a second place for the password to be wrong.
func pgSecretName() string { return namePostgres + "-app" }

func (k *Kube) workloads(ns string) []ctrlclient.Object {
	return []ctrlclient.Object{
		k.atlantisDeployment(ns),
		k.atlantisService(ns),
		k.signerDeployment(ns),
		k.signerService(ns),
	}
}

func one() *int32 { v := int32(1); return &v }

// fsGroup is how a non-root container reads its own secrets.
//
// A Secret volume's files are owned by root, so tightening the mode to 0400
// makes them unreadable by every image here, all of which run as a non-root
// user. The signer's failure is `load CA: read /ca-private/ca.crt: permission
// denied`, which reads like a missing file.
//
// fsGroup fixes it without needing the image's own uid: Kubernetes sets group
// ownership of the volume to this gid and adds it to the process's
// supplementary groups, so 0440 is readable by the container and nothing else.
//
// 65532 is the nonroot uid/gid distroless and most base images converge on. The
// value is arbitrary — the supplementary group does the work — and only has to
// be stable.
const fsGroupNonRoot int64 = 65532

// nonRootPodSecurity is the pod-level half of the hardening.
//
// runAsNonRoot without runAsUser: the kubelet refuses to start a container
// whose user is root whatever the image says, and Pod Security "restricted"
// requires it without requiring a uid. Naming a uid here would duplicate a
// number that already lives in the Dockerfile.
//
// The kubelet checks this from image metadata alone, before the container runs,
// so it cannot resolve a USER name to a uid and rejects any image that has one:
//
//	container has runAsNonRoot and image has non-numeric user (atlantis),
//	cannot verify user is non-root
//
// The pod then sits in CreateContainerConfigError, which does not name this
// field. Every Dockerfile in the fleet declares `USER <number>`.
//
// Seccomp is set explicitly: GKE Autopilot applies RuntimeDefault on its own
// and nothing else does.
func nonRootPodSecurity() *corev1.PodSecurityContext {
	g := fsGroupNonRoot
	yes := true
	return &corev1.PodSecurityContext{
		FSGroup:        &g,
		RunAsNonRoot:   &yes,
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// hardenedContainer is the container-level half.
//
// Capabilities are dropped entirely. Both binaries are Go servers listening on
// 9090, 8081 and 7070, all above 1024, so neither needs NET_BIND_SERVICE, and
// neither reads another process or changes file ownership.
//
// readOnlyRootFilesystem leaves the image as the binary and its data. Two write
// paths exist in the server and neither is reached from here: ATL_MIRROR_SCHEMA
// makes admin.mirror write submitted .atl files under /app/schema and defaults
// to false, and MIGRATIONS_DIR is unset. Setting either would need /app/schema
// to have a volume of its own.
//
// /tmp is the third, and is why writableTmp exists. Go's os.TempDir is /tmp and
// the embedded sandbox writes there, so a read-only root with nothing mounted
// boots cleanly and fails later under load. An emptyDir dies with the pod.
func hardenedContainer() *corev1.SecurityContext {
	no, yes := false, true
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: &no,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		ReadOnlyRootFilesystem:   &yes,
	}
}

// The two halves of the writable /tmp, kept next to each other because a mount
// without its volume is an unschedulable pod and the error names only the mount.
const tmpVolume = "tmp"

func tmpMount() corev1.VolumeMount {
	return corev1.VolumeMount{Name: tmpVolume, MountPath: "/tmp"}
}

func writableTmp() corev1.Volume {
	return corev1.Volume{
		Name:         tmpVolume,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}
}

// secretMode is 0440: readable by the owner and the fsGroup, nobody else.
func secretMode() *int32 { v := int32(0o440); return &v }

// appResources keeps both application pods out of the BestEffort class and
// bounds what either can take.
//
// Without requests they are first to be evicted under node pressure, so an
// organisation's atlantis dies because a different tenant on the node got busy.
//
// Memory is limited because it is incompressible: a pod that climbs without a
// limit drives the node out of memory, and the kernel picks its victim by OOM
// score, which may be another organisation's Postgres. CPU is left unlimited
// because the scheduler already shares it in proportion to requests, while a
// limit throttles even on an idle node. A busy tenant can therefore take spare
// capacity and slow its neighbours, but cannot push them below their requests.
//
// Limit equals request, so the scheduler reserves everything the pod may use.
// QoS stays Burstable; Guaranteed would need CPU limits, and eviction ranks on
// how far a pod exceeds its request, which one of these cannot do.
//
// High-water anonymous memory, read from each container's own cgroup and
// unchanged between a pod one minute old and one eighteen hours old:
//
//	atlantis   12.0Mi   against 256Mi
//	signer      6.9Mi   against  64Mi
//
// The requests are not cut to match: those figures cover idle, boot with
// migrations, and provisioning — not concurrent RPC load or a large apply.
func appResources(memory string) corev1.ResourceRequirements {
	q := resource.MustParse(memory)
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceMemory: q,
			corev1.ResourceCPU:    resource.MustParse("50m"),
		},
		// Memory only. Adding corev1.ResourceCPU here would throttle; see above.
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: q,
		},
	}
}

func selectorFor(component string) map[string]string {
	return map[string]string{"app.kubernetes.io/name": component}
}

func (k *Kube) atlantisDeployment(ns string) ctrlclient.Object {
	return &appsv1.Deployment{
		TypeMeta:   typeMeta("apps/v1", "Deployment"),
		ObjectMeta: k.meta(ns, nameAtlantis, nameAtlantis),
		Spec: appsv1.DeploymentSpec{
			Replicas: one(),
			Selector: &metav1.LabelSelector{MatchLabels: selectorFor(nameAtlantis)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: selectorFor(nameAtlantis)},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:            nameAtlantis,
						Image:           k.cfg.ServerImage,
						ImagePullPolicy: corev1.PullPolicy(k.cfg.PullPolicy),
						Env:             k.atlantisEnv(),
						Ports: []corev1.ContainerPort{
							{Name: "grpc", ContainerPort: portGRPC},
							{Name: "health", ContainerPort: portHealth},
						},
						// Readiness has real dependencies — the database, the
						// cache, and the outbox worker's freshness — so without
						// a probe the Service would route to a pod whose pool
						// is not up yet.
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/readyz",
									Port: intstr.FromInt32(portHealth),
									// The health listener terminates TLS, so that
									// /status and /metrics can demand a client
									// certificate. The kubelet holds none, which is
									// why those two routes are gated individually
									// and these two are not.
									//
									// The kubelet does not verify the server
									// certificate on a probe, so the organisation's
									// own authority needs no distribution here.
									Scheme: corev1.URISchemeHTTPS,
								},
							},
							InitialDelaySeconds: 5,
							PeriodSeconds:       5,
							FailureThreshold:    3,
						},
						// Liveness is /healthz, which answers without touching
						// anything downstream. Pointing liveness at /readyz
						// would restart the pod every time the database
						// hiccupped, turning a recoverable outage into a crash
						// loop.
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path:   "/healthz",
									Port:   intstr.FromInt32(portHealth),
									Scheme: corev1.URISchemeHTTPS,
								},
							},
							InitialDelaySeconds: 10,
							PeriodSeconds:       10,
							FailureThreshold:    6,
						},
						// AUTO_MIGRATE runs the whole migration set before the
						// health listener binds, so for the length of a first boot
						// against an empty database nothing answers on this port.
						// Liveness alone gives that 10s + 6×10s = 70 seconds, after
						// which the kubelet kills a container that is doing what it
						// should; the pod restarts, migrates from the top again and
						// is killed again, with events saying "Liveness probe
						// failed".
						//
						// While a startup probe is failing, liveness and readiness
						// are not run at all. So this is the boot budget and the
						// two above are the steady-state budget.
						//
						// 5 minutes: long enough for a cold migration on a busy
						// node, short enough that a wedged container is not left
						// for a quarter of an hour.
						StartupProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path:   "/healthz",
									Port:   intstr.FromInt32(portHealth),
									Scheme: corev1.URISchemeHTTPS,
								},
							},
							PeriodSeconds:    5,
							FailureThreshold: 60,
						},
						Resources:       appResources("256Mi"),
						SecurityContext: hardenedContainer(),
						VolumeMounts: []corev1.VolumeMount{
							{
								Name:      "tls",
								MountPath: mountAtlantisTLS,
								ReadOnly:  true,
							},
							tmpMount(),
						},
					}},
					SecurityContext: nonRootPodSecurity(),
					// Its own account with no API token mounted. The default
					// service account is mounted automatically, so without this a
					// process running a customer's SQL holds a live credential for
					// the Kubernetes API.
					ServiceAccountName:           saName,
					AutomountServiceAccountToken: boolPtr(false),
					// Shutdown takes about 30 seconds — the deploy guide says the
					// grace period must exceed it. The default is exactly 30, so a
					// rolling restart cuts the last of it off.
					TerminationGracePeriodSeconds: int64Ptr(40),
					Volumes: []corev1.Volume{
						{
							Name: "tls",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: secretAtlantisTLS,
									// The server's private key is in here. The
									// default 0644 would leave it readable by any
									// process in the pod.
									DefaultMode: secretMode(),
								},
							},
						},
						writableTmp(),
					},
				},
			},
		},
	}
}

// atlantisEnv is the server's configuration.
//
// atlantis verifies callers against ca.crt and never signs, so the issuing
// authority's private key is not mounted here.
func (k *Kube) atlantisEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		{
			// The DSN comes from CloudNativePG's generated Secret rather than
			// being composed here. It is the only place the password exists.
			Name: "PG_URL",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: pgSecretName()},
					Key:                  "uri",
				},
			},
		},
		{Name: "TLS_CERT_FILE", Value: mountAtlantisTLS + "/tls.crt"},
		{Name: "TLS_KEY_FILE", Value: mountAtlantisTLS + "/tls.key"},
		{Name: "TLS_CA_FILE", Value: mountAtlantisTLS + "/ca.crt"},

		// Set explicitly: the default localhost:11211 resolves to nothing in a
		// pod, and an unreachable cache is a permanent readiness failure rather
		// than a startup error, because /readyz performs a real cache operation.
		{Name: "MEMCACHED_ADDR", Value: k.cfg.MemcachedAddr},

		// The binary carries migrations/infra and nothing else applies them.
		// Defaults to false.
		{Name: "AUTO_MIGRATE", Value: "true"},

		// Refuse to start on a Community TimescaleDB build, which Dockerfile.pg
		// exists to avoid.
		{Name: "ATL_REQUIRE_APACHE_TIMESCALE", Value: "true"},

		// Refuse to start on a role that can bypass row-level security. Without
		// it every `partition by` policy is inert and silent: everything
		// appears to work and tenants can read each other.
		{Name: "ATL_REQUIRE_TENANT_ISOLATION", Value: "true"},

		{Name: "LOG_LEVEL", Value: "info"},
	}
}

func (k *Kube) atlantisService(ns string) ctrlclient.Object {
	return &corev1.Service{
		TypeMeta:   typeMeta("v1", "Service"),
		ObjectMeta: k.meta(ns, nameAtlantis, nameAtlantis),
		Spec: corev1.ServiceSpec{
			// NodePort because the console and every caller are outside the
			// cluster. The ports are allocated by Kubernetes and read back
			// afterwards rather than chosen here: a fixed port collides
			// eventually.
			Type:     corev1.ServiceTypeNodePort,
			Selector: selectorFor(nameAtlantis),
			Ports: []corev1.ServicePort{
				{Name: "grpc", Port: portGRPC, TargetPort: intstr.FromInt32(portGRPC)},
				{Name: "health", Port: portHealth, TargetPort: intstr.FromInt32(portHealth)},
			},
		},
	}
}

func (k *Kube) signerDeployment(ns string) ctrlclient.Object {
	return &appsv1.Deployment{
		TypeMeta:   typeMeta("apps/v1", "Deployment"),
		ObjectMeta: k.meta(ns, nameSigner, nameSigner),
		Spec: appsv1.DeploymentSpec{
			Replicas: one(),
			Selector: &metav1.LabelSelector{MatchLabels: selectorFor(nameSigner)},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: selectorFor(nameSigner)},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:            nameSigner,
						Image:           k.cfg.SignerImage,
						ImagePullPolicy: corev1.PullPolicy(k.cfg.PullPolicy),
						Env:             k.signerEnv(),
						Ports: []corev1.ContainerPort{
							{Name: "https", ContainerPort: portSigner},
							{Name: "health", ContainerPort: portSigner + 1},
						},
						// The signer's health listener is plaintext, so a probe
						// reaches it without holding a certificate.
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/healthz",
									Port: intstr.FromInt32(portSigner + 1),
								},
							},
							InitialDelaySeconds: 3,
							PeriodSeconds:       5,
						},
						Resources:       appResources("64Mi"),
						SecurityContext: hardenedContainer(),
						VolumeMounts: []corev1.VolumeMount{
							{
								Name:      "pki",
								MountPath: mountSignerPKI,
								ReadOnly:  true,
							},
							tmpMount(),
						},
					}},
					SecurityContext: nonRootPodSecurity(),
					// Its own account, with no API token mounted. Kubernetes
					// mounts the default account's token into every pod that does
					// not refuse it.
					ServiceAccountName:           saName,
					AutomountServiceAccountToken: boolPtr(false),
					// Shutdown takes about 30 seconds — the deploy guide says the
					// grace period must exceed it. The default is exactly 30, so a
					// rolling restart cuts the last of it off.
					TerminationGracePeriodSeconds: int64Ptr(40),
					Volumes: []corev1.Volume{
						{
							Name: "pki",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: secretSignerPKI,
									// The CA private key. The mount scope is the
									// boundary; the mode is a second layer, and
									// works only alongside the fsGroup above.
									DefaultMode: secretMode(),
								},
							},
						},
						writableTmp(),
					},
				},
			},
		},
	}
}

func (k *Kube) signerEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		{
			// The signer checks that a caller is registered before issuing, so
			// it needs this organisation's database — the same one atlantis
			// uses, and no other organisation's.
			Name: "PG_URL",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: pgSecretName()},
					Key:                  "uri",
				},
			},
		},
		// CA_DIR is a directory, and loadCA appends ca.crt and ca.key to it.
		{Name: "CA_DIR", Value: mountSignerPKI},
		{Name: "SIGNER_TLS_CERT", Value: mountSignerPKI + "/tls.crt"},
		{Name: "SIGNER_TLS_KEY", Value: mountSignerPKI + "/tls.key"},

		// Who may ask. Not the authority the signer issues from: every leaf it
		// signs is marked for client authentication, so a signer trusting its
		// own issuing root would accept everything it had produced as a
		// credential to itself.
		{Name: "SIGNER_CLIENT_CA", Value: mountSignerPKI + "/client-ca.crt"},
		{Name: "SIGNER_ALLOWED_CLIENT_CNS", Value: "atlantis-console"},

		// Bind on all interfaces. The documented example is 127.0.0.1, which in
		// a container is unreachable from anywhere else and presents as a
		// connection refused that looks like a crashed process.
		{Name: "SIGNER_LISTEN", Value: fmt.Sprintf(":%d", portSigner)},
		{Name: "SIGNER_HEALTH_LISTEN", Value: fmt.Sprintf(":%d", portSigner+1)},
	}
}

func (k *Kube) signerService(ns string) ctrlclient.Object {
	return &corev1.Service{
		TypeMeta:   typeMeta("v1", "Service"),
		ObjectMeta: k.meta(ns, nameSigner, nameSigner),
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeNodePort,
			Selector: selectorFor(nameSigner),
			Ports: []corev1.ServicePort{
				{Name: "https", Port: portSigner, TargetPort: intstr.FromInt32(portSigner)},
			},
		},
	}
}

// saName is the ServiceAccount both workloads run as.
//
// One per namespace rather than one per workload: neither is granted anything,
// and the account exists to stop the default one being mounted.
const saName = "atlantis"

func boolPtr(v bool) *bool    { return &v }
func int64Ptr(v int64) *int64 { return &v }
