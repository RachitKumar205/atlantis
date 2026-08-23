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
// Its `uri` key is a complete DSN, which is why nothing here composes one — a
// hand-built connection string is a second place for the password to be wrong.
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
// A Secret volume's files are owned by root, so tightening the mode to 0400 —
// the obvious way to protect a CA private key — makes them unreadable by every
// image here, all of which run as a non-root user. The signer's failure is
// `load CA: read /ca-private/ca.crt: permission denied`, which reads like a
// missing file rather than a mode we chose.
//
// fsGroup fixes it without needing to know the image's own uid: Kubernetes sets
// group ownership of the volume to this gid *and* adds it to the process's
// supplementary groups. So 0440 is readable by the container and by nothing
// else, whatever user the image happens to run as.
//
// 65532 is the nonroot uid/gid distroless and most base images converge on. The
// value is arbitrary here because the supplementary group is what does the
// work; it only has to be stable.
const fsGroupNonRoot int64 = 65532

func nonRootPodSecurity() *corev1.PodSecurityContext {
	g := fsGroupNonRoot
	return &corev1.PodSecurityContext{FSGroup: &g}
}

// secretMode is 0440: readable by the owner and the fsGroup, nobody else.
func secretMode() *int32 { v := int32(0o440); return &v }

// appResources keeps both application pods out of the BestEffort class.
//
// Without requests they are the first thing evicted under node pressure, which
// means an organisation's atlantis dies before its database notices anything is
// wrong — and on a shared node pool that is a tenant losing service because a
// different tenant got busy.
func appResources(memory string) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse(memory),
			corev1.ResourceCPU:    resource.MustParse("50m"),
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
						Resources: appResources("256Mi"),
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "tls",
							MountPath: mountAtlantisTLS,
							ReadOnly:  true,
						}},
					}},
					SecurityContext: nonRootPodSecurity(),
					Volumes: []corev1.Volume{{
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
					}},
				},
			},
		},
	}
}

// atlantisEnv is the server's configuration.
//
// Note what is absent: the issuing authority's private key. atlantis verifies
// callers against ca.crt and never signs anything, so it has no business
// holding the key that could mint one.
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

		// Set explicitly because the default is localhost:11211, which in a pod
		// resolves to nothing — and an unreachable cache is not a startup
		// error, it is a permanent readiness failure, because /readyz performs
		// a real cache operation.
		{Name: "MEMCACHED_ADDR", Value: k.cfg.MemcachedAddr},

		// The binary carries migrations/infra and nothing else applies them.
		// Defaults to false.
		{Name: "AUTO_MIGRATE", Value: "true"},

		// Refuse to start on a Community TimescaleDB build. Dockerfile.pg exists
		// to satisfy a licensing condition; this is what makes the condition
		// enforced rather than assumed.
		{Name: "ATL_REQUIRE_APACHE_TIMESCALE", Value: "true"},

		// Refuse to start on a role that can bypass row-level security. Without
		// it every `partition by` policy is inert and silent — the failure mode
		// where everything appears to work and tenants can read each other.
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
			// afterwards rather than chosen here — a port we pick is a port
			// that collides with somebody eventually.
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
						// The signer's health listener is plaintext precisely so
						// a probe can reach it without holding a certificate.
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
						Resources: appResources("64Mi"),
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "pki",
							MountPath: mountSignerPKI,
							ReadOnly:  true,
						}},
					}},
					SecurityContext: nonRootPodSecurity(),
					Volumes: []corev1.Volume{{
						Name: "pki",
						VolumeSource: corev1.VolumeSource{
							Secret: &corev1.SecretVolumeSource{
								SecretName: secretSignerPKI,
								// The CA private key is the most valuable thing
								// in the namespace. The mount scope is the real
								// boundary; the mode is the second answer, and
								// it only works alongside the fsGroup above.
								DefaultMode: secretMode(),
							},
						},
					}},
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

		// Who may ask, and it must not be the authority the signer issues from:
		// every leaf it signs is marked for client authentication, so a signer
		// trusting its own issuing root would accept everything it had ever
		// produced as a credential to itself.
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
