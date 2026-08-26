package provision

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/managedfields"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func testConfig() Config {
	return Config{
		ExternalHost:  "atl-dev.test",
		ServerImage:   "atlantis-server:local",
		SignerImage:   "atlantis-signer:local",
		PostgresImage: "atlantis-pg:17.11",
		MemcachedAddr: "memcached.atlantis-system:11211",
		StorageClass:  "local-path",
	}
}

func newTestKube(t *testing.T, objs ...ctrlclient.Object) *Kube {
	t.Helper()
	scheme, err := NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	// The deduced converter, explicitly. The fake client's default converter
	// chain expects generated apply configurations, which exist for the
	// built-in types and not for CloudNativePG's Cluster — and the mismatch
	// surfaces as "expected objects with types from the same schema" rather
	// than as anything mentioning schemas it could not find.
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithTypeConverters(managedfields.NewDeducedTypeConverter()).
		WithObjects(objs...).
		Build()
	k, err := NewKube(testConfig(), c, nil)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func pkiSecretOf(t *testing.T, k *Kube, ns string) *corev1.Secret {
	t.Helper()
	var s corev1.Secret
	if err := k.c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: secretPKI}, &s); err != nil {
		t.Fatalf("read %s: %v", secretPKI, err)
	}
	return &s
}

func caSerial(t *testing.T, pemBytes []byte) string {
	t.Helper()
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		t.Fatal("no PEM block")
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c.SerialNumber.String()
}

// Provisioning is interrupted routinely — a restart, a timeout, a full disk —
// so the second run has to converge rather than duplicate. A fresh CA would
// invalidate every caller certificate already issued for the organisation, and
// nothing would report it.
func TestEnsureIsIdempotentAndKeepsTheAuthority(t *testing.T) {
	k := newTestKube(t)
	ctx := context.Background()

	if _, err := k.Ensure(ctx, Spec{Org: "acme"}); err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	ns := k.cfg.Namespace("acme")
	first := caSerial(t, pkiSecretOf(t, k, ns).Data["ca.crt"])

	if _, err := k.Ensure(ctx, Spec{Org: "acme"}); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	second := caSerial(t, pkiSecretOf(t, k, ns).Data["ca.crt"])

	if first != second {
		t.Fatalf("the authority was replaced on the second run: %s then %s", first, second)
	}
}

// A presence check cannot see this. Something that exists and is unusable,
// treated as "already done", makes every retry a no-op while the organisation
// stays broken.
func TestUnusableStoredCertificatesAreRefusedRatherThanReused(t *testing.T) {
	k := newTestKube(t)
	ctx := context.Background()

	if _, err := k.Ensure(ctx, Spec{Org: "acme"}); err != nil {
		t.Fatal(err)
	}
	ns := k.cfg.Namespace("acme")

	corrupt := pkiSecretOf(t, k, ns)
	corrupt.Data["ca.key"] = []byte("-----BEGIN PRIVATE KEY-----\nnope\n-----END PRIVATE KEY-----\n")
	if err := k.c.Update(ctx, corrupt); err != nil {
		t.Fatal(err)
	}

	_, err := k.Ensure(ctx, Spec{Org: "acme"})
	if err == nil {
		t.Fatal("Ensure reused certificates it could not parse")
	}
	// The message has to say what to do about it. This state does not
	// self-heal, and the fix costs every caller a re-enrolment.
	if !strings.Contains(err.Error(), secretPKI) {
		t.Errorf("the error does not name the secret to delete: %v", err)
	}
}

// A missing half is as bad as a corrupt one, and is what a truncated write
// leaves behind.
func TestAPartiallyWrittenSecretIsRefused(t *testing.T) {
	k := newTestKube(t)
	ctx := context.Background()

	if _, err := k.Ensure(ctx, Spec{Org: "acme"}); err != nil {
		t.Fatal(err)
	}
	ns := k.cfg.Namespace("acme")

	partial := pkiSecretOf(t, k, ns)
	delete(partial.Data, "signer-client.key")
	if err := k.c.Update(ctx, partial); err != nil {
		t.Fatal(err)
	}

	if _, err := k.Ensure(ctx, Spec{Org: "acme"}); err == nil {
		t.Fatal("Ensure accepted a bundle with a missing key")
	}
}

// atlantis verifies callers against the authority and never signs, so an
// atlantis holding the CA key could issue itself any caller identity in the
// organisation.
func TestTheAtlantisPodNeverHoldsTheAuthorityKey(t *testing.T) {
	k := newTestKube(t)
	ctx := context.Background()
	if _, err := k.Ensure(ctx, Spec{Org: "acme"}); err != nil {
		t.Fatal(err)
	}
	ns := k.cfg.Namespace("acme")

	var d appsv1.Deployment
	if err := k.c.Get(ctx, types.NamespacedName{Namespace: ns, Name: nameAtlantis}, &d); err != nil {
		t.Fatal(err)
	}
	for _, v := range d.Spec.Template.Spec.Volumes {
		if v.Secret == nil {
			continue
		}
		if v.Secret.SecretName == secretSignerPKI || v.Secret.SecretName == secretPKI {
			t.Fatalf("the atlantis pod mounts %q, which carries the CA private key", v.Secret.SecretName)
		}
	}

	// And the secret it does mount must not contain the key either — a mount
	// of the right name holding the wrong contents is the same failure.
	var s corev1.Secret
	if err := k.c.Get(ctx, types.NamespacedName{Namespace: ns, Name: secretAtlantisTLS}, &s); err != nil {
		t.Fatal(err)
	}
	for key := range s.Data {
		if key == "ca.key" || key == "signer-ca.key" {
			t.Errorf("the atlantis TLS secret carries %q", key)
		}
	}
}

// loadCA reads exactly $CA_DIR/ca.crt and $CA_DIR/ca.key. These are not free
// choices, and getting one wrong is a boot failure reporting a missing file
// rather than a naming mistake.
func TestTheSignerMountMatchesWhatLoadCAReads(t *testing.T) {
	k := newTestKube(t)
	ctx := context.Background()
	if _, err := k.Ensure(ctx, Spec{Org: "acme"}); err != nil {
		t.Fatal(err)
	}
	ns := k.cfg.Namespace("acme")

	var s corev1.Secret
	if err := k.c.Get(ctx, types.NamespacedName{Namespace: ns, Name: secretSignerPKI}, &s); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ca.crt", "ca.key"} {
		if len(s.Data[want]) == 0 {
			t.Errorf("the signer secret has no %q, which is the filename loadCA reads", want)
		}
	}

	var d appsv1.Deployment
	if err := k.c.Get(ctx, types.NamespacedName{Namespace: ns, Name: nameSigner}, &d); err != nil {
		t.Fatal(err)
	}
	env := envOf(d)
	if env["CA_DIR"] != mountSignerPKI {
		t.Errorf("CA_DIR is %q, mount is %q", env["CA_DIR"], mountSignerPKI)
	}
	// The documented example binds loopback, which in a pod is unreachable and
	// presents as a connection refused that looks like a crashed process.
	if strings.Contains(env["SIGNER_LISTEN"], "127.0.0.1") {
		t.Errorf("SIGNER_LISTEN binds loopback: %q", env["SIGNER_LISTEN"])
	}
}

// Settings whose defaults are wrong for a hosted deployment, each of which
// fails silently rather than loudly if it is left alone.
func TestTheAtlantisDeploymentSetsWhatTheDefaultsGetWrong(t *testing.T) {
	k := newTestKube(t)
	ctx := context.Background()
	if _, err := k.Ensure(ctx, Spec{Org: "acme"}); err != nil {
		t.Fatal(err)
	}

	var d appsv1.Deployment
	if err := k.c.Get(ctx, types.NamespacedName{
		Namespace: k.cfg.Namespace("acme"), Name: nameAtlantis,
	}, &d); err != nil {
		t.Fatal(err)
	}
	env := envOf(d)

	for _, tc := range []struct{ key, want, why string }{
		{"AUTO_MIGRATE", "true", "nothing else applies migrations/infra"},
		{"ATL_REQUIRE_APACHE_TIMESCALE", "true", "a Community build is a licensing problem, not a runtime one"},
		{"ATL_REQUIRE_TENANT_ISOLATION", "true", "without it every partition policy is inert and silent"},
		{"MEMCACHED_ADDR", k.cfg.MemcachedAddr, "the default is localhost, which in a pod never becomes Ready"},
	} {
		if env[tc.key] != tc.want {
			t.Errorf("%s = %q, want %q — %s", tc.key, env[tc.key], tc.want, tc.why)
		}
	}

	// Readiness has real dependencies, so without a probe the Service routes to
	// a pod whose pool is not up.
	if d.Spec.Template.Spec.Containers[0].ReadinessProbe == nil {
		t.Error("the atlantis container has no readiness probe")
	}
	if d.Spec.Template.Spec.Containers[0].Resources.Requests.Memory().IsZero() {
		t.Error("the atlantis container has no memory request and is therefore BestEffort")
	}
}

// The policy that isolates the database must not block the controller that
// creates it: the Cluster never becomes ready, with nothing in its status
// pointing at the network.
func TestTheNetworkPolicyAdmitsTheOperator(t *testing.T) {
	k := newTestKube(t)
	ctx := context.Background()
	if _, err := k.Ensure(ctx, Spec{Org: "acme"}); err != nil {
		t.Fatal(err)
	}

	var list networkingv1.NetworkPolicyList
	if err := k.c.List(ctx, &list, ctrlclient.InNamespace(k.cfg.Namespace("acme"))); err != nil {
		t.Fatal(err)
	}

	var admitsOperator bool
	for _, p := range list.Items {
		for _, rule := range p.Spec.Ingress {
			for _, peer := range rule.From {
				if peer.NamespaceSelector == nil {
					continue
				}
				if peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] == k.cfg.OperatorNamespace {
					admitsOperator = true
				}
			}
		}
	}
	if !admitsOperator {
		t.Fatalf("no policy admits %s; CloudNativePG cannot reconcile the cluster it created",
			k.cfg.OperatorNamespace)
	}
}

// Outside the cluster is allowed, inside it is not: that lets the console and a
// caller in while keeping every other tenant's pods out, and a pod cannot forge
// a source address outside the pod CIDR.
func TestTheExternalPolicyExcludesThePodNetwork(t *testing.T) {
	k := newTestKube(t)
	ctx := context.Background()
	if _, err := k.Ensure(ctx, Spec{Org: "acme"}); err != nil {
		t.Fatal(err)
	}

	var list networkingv1.NetworkPolicyList
	if err := k.c.List(ctx, &list, ctrlclient.InNamespace(k.cfg.Namespace("acme"))); err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, p := range list.Items {
		for _, rule := range p.Spec.Ingress {
			for _, peer := range rule.From {
				if peer.IPBlock == nil {
					continue
				}
				found = true
				if len(peer.IPBlock.Except) == 0 {
					t.Error("an ipBlock rule admits the whole internet including every pod")
				}
				for _, e := range peer.IPBlock.Except {
					if e != k.cfg.PodCIDR {
						t.Errorf("ipBlock excludes %q, not the pod network %q", e, k.cfg.PodCIDR)
					}
				}
			}
		}
	}
	if !found {
		t.Fatal("no policy admits traffic from outside the cluster; the console cannot reach this org")
	}
}

// A restrictive mode on a Secret volume is unreadable without an fsGroup: the
// files are owned by root and every image here runs as a non-root user, so
// tightening the mode alone produces `permission denied` on a file that is
// plainly there.
//
// Checked over every workload, since the next pod to mount a secret meets the
// same thing.
func TestARestrictiveSecretModeAlwaysComesWithAnFsGroup(t *testing.T) {
	k := newTestKube(t)
	ctx := context.Background()
	if _, err := k.Ensure(ctx, Spec{Org: "acme"}); err != nil {
		t.Fatal(err)
	}

	var deployments appsv1.DeploymentList
	if err := k.c.List(ctx, &deployments, ctrlclient.InNamespace(k.cfg.Namespace("acme"))); err != nil {
		t.Fatal(err)
	}
	if len(deployments.Items) == 0 {
		t.Fatal("no deployments to check")
	}

	for _, d := range deployments.Items {
		spec := d.Spec.Template.Spec
		for _, v := range spec.Volumes {
			if v.Secret == nil || v.Secret.DefaultMode == nil {
				continue
			}
			// World-readable needs no group; anything tighter does.
			if *v.Secret.DefaultMode&0o004 != 0 {
				continue
			}
			if spec.SecurityContext == nil || spec.SecurityContext.FSGroup == nil {
				t.Errorf("%s mounts %q with mode %#o and sets no fsGroup: "+
					"the container cannot read its own secret",
					d.Name, v.Secret.SecretName, *v.Secret.DefaultMode)
			}
		}
	}
}

func TestConfigRefusesAnIncompleteDeployment(t *testing.T) {
	scheme, err := NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	if _, err := NewKube(Config{}, c, nil); err == nil {
		t.Error("NewKube accepted an empty config")
	}

	// MemcachedAddr specifically: cmd/server has a default for it, and that
	// default is wrong in a pod.
	cfg := testConfig()
	cfg.MemcachedAddr = ""
	if _, err := NewKube(cfg, c, nil); err == nil {
		t.Error("NewKube accepted a config with no memcached address")
	}

	if _, err := NewKube(testConfig(), nil, nil); err == nil {
		t.Error("NewKube accepted a nil client")
	}
}

func TestDestroyRemovesTheNamespace(t *testing.T) {
	k := newTestKube(t)
	ctx := context.Background()
	if _, err := k.Ensure(ctx, Spec{Org: "acme"}); err != nil {
		t.Fatal(err)
	}
	if err := k.Destroy(ctx, "acme"); err != nil {
		t.Fatal(err)
	}

	var ns corev1.Namespace
	err := k.c.Get(ctx, types.NamespacedName{Name: k.cfg.Namespace("acme")}, &ns)
	if err == nil && ns.DeletionTimestamp == nil {
		t.Error("the namespace survived Destroy")
	}

	// Destroying something that is already gone is not an error: a retry after
	// a partial failure has to be able to finish the job.
	if err := k.Destroy(ctx, "acme"); err != nil {
		t.Errorf("second Destroy: %v", err)
	}
}

func envOf(d appsv1.Deployment) map[string]string {
	out := map[string]string{}
	for _, e := range d.Spec.Template.Spec.Containers[0].Env {
		out[e.Name] = e.Value
	}
	return out
}
