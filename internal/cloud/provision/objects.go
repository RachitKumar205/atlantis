package provision

import (
	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/rachitkumar205/atlantis/internal/cloud/provision/certs"
)

// labelOrg marks every object as belonging to one organisation, so a human
// debugging a cluster can select across namespaces and a policy can select
// within one.
const labelOrg = "atlantis.dev/org"

// Every object carries TypeMeta explicitly.
//
// Server-side apply is a patch against a named resource, so the API server has
// to be told what kind of thing it is being sent. A typed client can often
// infer it from the scheme, but the inference is silent when it fails and the
// resulting error names a field rather than the missing kind.
func typeMeta(apiVersion, kind string) metav1.TypeMeta {
	return metav1.TypeMeta{APIVersion: apiVersion, Kind: kind}
}

func (k *Kube) meta(ns, name, component string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:      name,
		Namespace: ns,
		Labels: map[string]string{
			"app.kubernetes.io/name":       component,
			"app.kubernetes.io/managed-by": "atlantis-provisioner",
		},
	}
}

// The three Pod Security Admission labels. Setting the security context on our
// own pods hardens the pods we write; the label is what makes the namespace
// refuse a pod that lacks it — including one nothing here wrote.
//
// enforce is what actually rejects. warn and audit are set to the same level
// because otherwise a rejection is a bare admission error with no record of it;
// audit puts the reason in the API server's log, and warn returns it to whoever
// applied the object.
//
// "restricted" rather than "baseline": baseline permits running as root, and
// the whole point of the pod-level context here is that this namespace runs a
// stranger's queries.
const (
	psaEnforce = "pod-security.kubernetes.io/enforce"
	psaAudit   = "pod-security.kubernetes.io/audit"
	psaWarn    = "pod-security.kubernetes.io/warn"
	psaLevel   = "restricted"
)

func (k *Kube) namespace(ns, org string) ctrlclient.Object {
	return &corev1.Namespace{
		TypeMeta: typeMeta("v1", "Namespace"),
		ObjectMeta: metav1.ObjectMeta{
			Name: ns,
			Labels: map[string]string{
				labelOrg:                       org,
				"app.kubernetes.io/managed-by": "atlantis-provisioner",
				psaEnforce:                     psaLevel,
				psaAudit:                       psaLevel,
				psaWarn:                        psaLevel,
			},
		},
	}
}

// serviceAccount exists so the workloads have something to run as other than
// `default`.
//
// It is granted nothing — no Role, no RoleBinding — and that is the whole
// design. Kubernetes mounts a token for the default account into every pod that
// does not say otherwise, so before this the atlantis pod, which runs a
// customer's SQL, held a live API credential it had no use for. The account
// separates "this pod's identity" from "the namespace's identity" so that
// granting the second later does not silently grant the first.
func (k *Kube) serviceAccount(ns string) ctrlclient.Object {
	no := false
	return &corev1.ServiceAccount{
		TypeMeta:                     typeMeta("v1", "ServiceAccount"),
		ObjectMeta:                   k.meta(ns, saName, saName),
		AutomountServiceAccountToken: &no,
	}
}

// networkPolicies is four policies rather than one, because they answer four
// different questions and a single policy that answered all of them would be
// unreadable.
//
// Policies are additive — a pod is reachable if any policy admits the traffic —
// so the baseline sets the floor and the other three open exactly what has to be
// open: a caller from outside, the console from within the control plane, and
// CloudNativePG for the database it manages.
func (k *Kube) networkPolicies(ns string) []ctrlclient.Object {
	proto := corev1.ProtocolTCP
	port := func(p int32) networkingv1.NetworkPolicyPort {
		v := intstr.FromInt32(p)
		return networkingv1.NetworkPolicyPort{Protocol: &proto, Port: &v}
	}

	// 1. Nothing reaches a pod in this namespace except another pod in it.
	//
	// Ingress only. Adding Egress here would default-deny outbound too, and the
	// first casualty is DNS to kube-system — after which nothing resolves the
	// Postgres service and the failure looks like a database problem.
	baseline := &networkingv1.NetworkPolicy{
		TypeMeta:   typeMeta("networking.k8s.io/v1", "NetworkPolicy"),
		ObjectMeta: k.meta(ns, "baseline", "network"),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}},
			}},
		},
	}

	// 2. The services an organisation is reached at, from outside the cluster.
	//
	// "Outside the cluster" is expressed as the whole internet minus the pod
	// network, which is what distinguishes a caller from another tenant's pod. A
	// pod cannot forge a source address outside the pod CIDR, so this is a real
	// boundary rather than a convention.
	//
	// This used to say "the console and a caller — both off-cluster". That
	// stopped being true when the console moved into the cluster: its traffic
	// now comes from inside the pod CIDR, which this rule excludes by design, so
	// it was refused here after resolving perfectly well. Dropped rather than
	// rejected, so it presented as a page that loaded forever and then timed
	// out. Policy 3 is what lets it in.
	//
	// It covers the health port too. That port is not harmless: /status and
	// /metrics live on the same listener as /healthz and are unauthenticated by
	// design, disclosing schema version, build version and per-caller RPC
	// counts. cmd/server/health.go says they are "already exposed at the
	// platform layer" — this is that layer.
	external := &networkingv1.NetworkPolicy{
		TypeMeta:   typeMeta("networking.k8s.io/v1", "NetworkPolicy"),
		ObjectMeta: k.meta(ns, "external-access", "network"),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key:      "app.kubernetes.io/name",
					Operator: metav1.LabelSelectorOpIn,
					Values:   []string{nameAtlantis, nameSigner},
				}},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					IPBlock: &networkingv1.IPBlock{
						CIDR:   "0.0.0.0/0",
						Except: []string{k.cfg.PodCIDR},
					},
				}},
				Ports: []networkingv1.NetworkPolicyPort{
					port(portGRPC), port(portHealth), port(portSigner),
				},
			}},
		},
	}

	// 3. The console reaches this organisation.
	//
	// Narrow on purpose: the control-plane namespace AND the console's own pod
	// label, in a single peer so the two are an AND rather than an OR. A
	// namespace-only rule would admit anything that happens to run beside the
	// console — Cloud, the provisioner, a debugging shell — none of which has
	// business on a tenant's admin port.
	//
	// This does not weaken tenant isolation. Another organisation's pods carry
	// neither the namespace nor the label, so policy 2's exclusion of the pod
	// CIDR still refuses them, and the k8s tests that prove one tenant cannot
	// reach another go on proving it.
	//
	// Harmless when the console runs outside the cluster: the selector matches
	// no pod and the rule admits nobody. That is why it is unconditional rather
	// than keyed to ConsoleInCluster — a policy that has to agree with a
	// separate setting is a policy that will disagree with it.
	console := &networkingv1.NetworkPolicy{
		TypeMeta:   typeMeta("networking.k8s.io/v1", "NetworkPolicy"),
		ObjectMeta: k.meta(ns, "console-access", "network"),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{{
					Key:      "app.kubernetes.io/name",
					Operator: metav1.LabelSelectorOpIn,
					Values:   []string{nameAtlantis, nameSigner},
				}},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					// One peer, both selectors: namespace AND pod. Two peers
					// would be OR, which is the whole namespace.
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							// Set by Kubernetes on every namespace, so it needs
							// nothing of ours to be labelled correctly.
							"kubernetes.io/metadata.name": k.cfg.ControlPlaneNamespace,
						},
					},
					PodSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app.kubernetes.io/name": "atlantis-console"},
					},
				}},
				Ports: []networkingv1.NetworkPolicyPort{
					port(portGRPC), port(portHealth), port(portSigner),
				},
			}},
		},
	}

	// 4. CloudNativePG reaches its own instances.
	//
	// Without this the baseline blocks the operator, and the symptom is the
	// worst kind: the Cluster never becomes ready, with nothing in its status
	// pointing at the network. The policy that isolates the database would be
	// the thing preventing the database from existing.
	operator := &networkingv1.NetworkPolicy{
		TypeMeta:   typeMeta("networking.k8s.io/v1", "NetworkPolicy"),
		ObjectMeta: k.meta(ns, "operator-access", "network"),
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"cnpg.io/cluster": namePostgres},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{{
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"kubernetes.io/metadata.name": k.cfg.OperatorNamespace,
						},
					},
				}},
			}},
		},
	}

	return []ctrlclient.Object{baseline, external, console, operator}
}

// pkiSecret is the source of truth for this organisation's certificates.
//
// Mounted by nothing. It exists so a repeated Ensure can find the authority it
// already minted rather than making a new one, and it holds both private keys —
// which is why it is separate from the secrets that are actually mounted, and
// why nothing mounts it.
func (k *Kube) pkiSecret(ns string, b *certs.Bundle) ctrlclient.Object {
	return &corev1.Secret{
		TypeMeta:   typeMeta("v1", "Secret"),
		ObjectMeta: k.meta(ns, secretPKI, "pki"),
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"ca.crt":            b.CA.CertPEM,
			"ca.key":            b.CA.KeyPEM,
			"signer-ca.crt":     b.SignerCA.CertPEM,
			"signer-ca.key":     b.SignerCA.KeyPEM,
			"server.crt":        b.Server.CertPEM,
			"server.key":        b.Server.KeyPEM,
			"console.crt":       b.Console.CertPEM,
			"console.key":       b.Console.KeyPEM,
			"signer-server.crt": b.SignerServer.CertPEM,
			"signer-server.key": b.SignerServer.KeyPEM,
			"signer-client.crt": b.SignerClient.CertPEM,
			"signer-client.key": b.SignerClient.KeyPEM,
		},
	}
}

// derivedSecrets are the projections each workload actually mounts.
//
// Split by who may read what. The atlantis pod gets its serving identity and
// the root it verifies callers against, and **not** the authority's private
// key — that is the single most important line in this file, because a
// compromised atlantis with the CA key can mint any caller in the organisation.
func (k *Kube) derivedSecrets(ns string, b *certs.Bundle) []ctrlclient.Object {
	atlantisTLS := &corev1.Secret{
		TypeMeta:   typeMeta("v1", "Secret"),
		ObjectMeta: k.meta(ns, secretAtlantisTLS, nameAtlantis),
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"tls.crt": b.Server.CertPEM,
			"tls.key": b.Server.KeyPEM,
			"ca.crt":  b.CA.CertPEM,
		},
	}

	// The signer's mount, and the filenames are not free choices: loadCA reads
	// exactly $CA_DIR/ca.crt and $CA_DIR/ca.key. Renaming either is a boot
	// failure that reports a missing file rather than a naming mistake.
	signerPKI := &corev1.Secret{
		TypeMeta:   typeMeta("v1", "Secret"),
		ObjectMeta: k.meta(ns, secretSignerPKI, nameSigner),
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"ca.crt":        b.CA.CertPEM,
			"ca.key":        b.CA.KeyPEM,
			"tls.crt":       b.SignerServer.CertPEM,
			"tls.key":       b.SignerServer.KeyPEM,
			"client-ca.crt": b.SignerCA.CertPEM,
		},
	}

	// Read once, by registration. Mounted by nothing.
	consoleCreds := &corev1.Secret{
		TypeMeta:   typeMeta("v1", "Secret"),
		ObjectMeta: k.meta(ns, secretConsoleCreds, "console"),
		Type:       corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"ca.crt":            b.CA.CertPEM,
			"tls.crt":           b.Console.CertPEM,
			"tls.key":           b.Console.KeyPEM,
			"signer-ca.crt":     b.SignerCA.CertPEM,
			"signer-client.crt": b.SignerClient.CertPEM,
			"signer-client.key": b.SignerClient.KeyPEM,
		},
	}

	return []ctrlclient.Object{atlantisTLS, signerPKI, consoleCreds}
}

// postgres is this organisation's database.
//
// One cluster per organisation is a hard constraint rather than a cost choice:
// write-ahead logging is cluster-wide, so point-in-time recovery is per
// cluster. "Restore us to 14:00" cannot be honoured on a shared cluster without
// rolling back every other tenant, and for a product whose pitch is safe schema
// change that is disqualifying.
func (k *Kube) postgres(ns string) ctrlclient.Object {
	storageClass := k.cfg.StorageClass
	var scPtr *string
	if storageClass != "" {
		scPtr = &storageClass
	}

	return &cnpgv1.Cluster{
		TypeMeta:   typeMeta("postgresql.cnpg.io/v1", "Cluster"),
		ObjectMeta: k.meta(ns, namePostgres, "postgres"),
		Spec: cnpgv1.ClusterSpec{
			Instances:       int(k.cfg.PostgresInstances),
			ImageName:       k.cfg.PostgresImage,
			ImagePullPolicy: corev1.PullPolicy(k.cfg.PullPolicy),

			// TimescaleDB has to be preloaded by the server, not merely
			// installed: CREATE EXTENSION fails without it. This is a top-level
			// list rather than a postgresql.parameters entry — CNPG merges it
			// with its own defaults, and putting it in parameters is rejected.
			PostgresConfiguration: cnpgv1.PostgresConfiguration{
				AdditionalLibraries: []string{"timescaledb"},
			},

			Bootstrap: &cnpgv1.BootstrapConfiguration{
				InitDB: &cnpgv1.BootstrapInitDB{
					// Named explicitly. CloudNativePG's default database is
					// "app", and leaving this unset points PG_URL at a database
					// the extensions below were never created in — which
					// surfaces as atlantis failing to open a pool, a long way
					// from the omission.
					Database: "atlantis",
					Owner:    "atlantis",

					// Bootstrap SQL runs as superuser during initialisation,
					// which is the only place these can run: pgvector is not a
					// trusted extension, so the application role cannot create
					// it, and the role is deliberately not a superuser.
					//
					// vector is the one atlantis cannot start without — the
					// connection pool registers pgvector types on every
					// connection and fails the connection if the type is
					// absent.
					PostInitApplicationSQL: []string{
						"CREATE EXTENSION IF NOT EXISTS vector",
						"CREATE EXTENSION IF NOT EXISTS citext",
						"CREATE EXTENSION IF NOT EXISTS timescaledb",
					},
				},
			},

			StorageConfiguration: cnpgv1.StorageConfiguration{
				Size:         k.cfg.PostgresStorage,
				StorageClass: scPtr,
			},

			// Memory is limited, CPU is not. See appResources in workloads.go
			// for why the two are treated differently.
			//
			// # Why 256Mi is a safe ceiling despite a measured 697Mi peak
			//
			// The cgroup high-water mark on an organisation running for
			// eighteen hours reads about 697Mi, which looks like this limit is
			// less than a third of what Postgres needs. Setting it from that
			// number would mean 700Mi per tenant and a third of the density.
			// Reading it as a requirement would be wrong:
			//
			//	anon   54.1Mi     file  372.4Mi     slab 12.5Mi   (18 hours old)
			//	anon   55.4Mi     file   30.6Mi     slab  3.2Mi   (one minute old)
			//
			// Anonymous memory — the part that cannot be reclaimed and the part
			// an OOM kill is decided on — is 55Mi, and it is the *same* on a
			// fresh pod as on an old one. The rest is page cache, which Postgres
			// lets grow to fill whatever it is given and which the kernel
			// reclaims before killing anything. The peak measures how much room
			// the cgroup had, not how much the database needs.
			//
			// So this caps the cache rather than starving the server: 55Mi of
			// working set stays resident, roughly 190Mi remains for cache, and
			// pressure reclaims instead of killing.
			//
			// 69 MiB was the figure the request was originally sized from, and
			// it matches the 55Mi anon plus 12Mi slab measured here — the
			// original estimate was right, and it is now checked rather than
			// remembered.
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					// The load-bearing number for density: Kubernetes schedules
					// on requests, not usage, so this decides how many
					// organisations fit on a node.
					corev1.ResourceMemory: resource.MustParse("256Mi"),
					corev1.ResourceCPU:    resource.MustParse("100m"),
				},
				Limits: corev1.ResourceList{
					corev1.ResourceMemory: resource.MustParse("256Mi"),
				},
			},
		},
	}
}
