package provision

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
)

// Provisioning against a real cluster.
//
// Everything in kube_test.go runs against a fake client, which answers what the
// API server would answer about the shape of an object and nothing at all about
// whether the resulting pods start. Every defect this package has had so far —
// a CA key the signer could not parse, a memcached address that makes readiness
// fail forever, a default database the extensions were not created in — is
// invisible to a fake and obvious here.
//
// Run with:
//
//	make dev-k8s
//	ATLANTIS_TEST_K8S=1 go test ./internal/cloud/provision/ -run K8s -v
func TestK8sProvisionsAWorkingOrganisation(t *testing.T) {
	if os.Getenv("ATLANTIS_TEST_K8S") == "" {
		t.Skip("set ATLANTIS_TEST_K8S to provision against a real cluster")
	}

	cfg, err := ctrlconfig.GetConfig()
	if err != nil {
		t.Fatalf("no cluster configuration: %v", err)
	}
	scheme, err := NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	c, err := ctrlclient.New(cfg, ctrlclient.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}

	pcfg := testConfig()
	pcfg.ReadyTimeout = 6 * time.Minute
	// The shared cache lives beside the operators rather than in a tenant
	// namespace. Nothing has created it at this point in the effort, which is
	// exactly what this test is here to reveal.
	pcfg.MemcachedAddr = "memcached.atlantis-system.svc.cluster.local:11211"

	k, err := NewKube(pcfg, c, nil)
	if err != nil {
		t.Fatal(err)
	}

	const org = "itest"
	ctx := context.Background()
	t.Cleanup(func() {
		if os.Getenv("ATLANTIS_TEST_K8S_KEEP") != "" {
			t.Logf("keeping namespace %s", k.cfg.Namespace(org))
			return
		}
		if err := k.Destroy(context.Background(), org); err != nil {
			t.Logf("destroy: %v", err)
		}
	})

	status, err := k.Ensure(ctx, Spec{Org: org})
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	t.Logf("endpoint=%s health=%s signer=%s ready=%v",
		status.Endpoint, status.HealthAddr, status.SignerAddr, status.Ready)

	if err := k.WaitReady(ctx, org); err != nil {
		// Dump what is actually wrong rather than only the timeout, because a
		// timeout here is almost never about time.
		t.Log(describe(ctx, t, k, org))
		t.Fatalf("WaitReady: %v", err)
	}

	// Re-read: WaitReady only proves the workloads are up, and the addresses
	// are allocated separately.
	status, err = k.Ensure(ctx, Spec{Org: org})
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if !status.Ready {
		t.Fatalf("still not ready after WaitReady returned: %+v", status)
	}

	// The acceptance check: atlantis answers from outside the cluster. This
	// passing means the pool opened, which means the extensions were created in
	// the database PG_URL points at, and that memcached is reachable — /readyz
	// probes it and 503s otherwise.
	//
	// https, and the certificate deliberately unverified. The health listener
	// terminates TLS so that /status and /metrics can demand a client
	// certificate; /readyz needs none, but it shares the listener, so the scheme
	// moved with it. This test holds no copy of the organisation's authority and
	// should not need one — it is asking "is this process serving", not "is this
	// the right process".
	t.Run("readyz from the host", func(t *testing.T) {
		url := fmt.Sprintf("https://%s/readyz", status.HealthAddr)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		client := &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // see above
			},
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", url, resp.StatusCode, body)
		}
	})

	// Every assertion here reads the *running pod*, not the Deployment and not
	// the structs in workloads.go.
	//
	// The distinction is the whole reason this subtest exists. A security
	// context is easy to write and easy to have no effect: `runAsNonRoot: true`
	// against an image with a named USER is refused by the kubelet at container
	// creation, so the field is present, correct, and the workload never starts —
	// and the Deployment still reads exactly right. Reading it back from the API
	// server after the pod is Running is the only form of this check that can
	// fail for the reasons it is meant to catch.
	//
	// The service-account token is the sharpest of them: nothing we write says
	// "no token volume". Admission adds a `kube-api-access-*` projected volume to
	// every pod that does not refuse it, so its *absence* is evidence about what
	// the cluster did, and it cannot be established anywhere but here.
	t.Run("the workload pods are hardened in the cluster", func(t *testing.T) {
		ns := k.cfg.Namespace(org)

		var ps corev1.PodList
		if err := c.List(ctx, &ps, ctrlclient.InNamespace(ns)); err != nil {
			t.Fatal(err)
		}
		seen := 0
		for i := range ps.Items {
			p := &ps.Items[i]
			name := p.Labels["app.kubernetes.io/name"]
			if name != nameAtlantis && name != nameSigner {
				continue // CloudNativePG's pods are its own to harden.
			}
			seen++
			if p.Status.Phase != corev1.PodRunning {
				t.Errorf("%s: phase %s, so nothing below is evidence", p.Name, p.Status.Phase)
			}

			sc := p.Spec.SecurityContext
			switch {
			case sc == nil:
				t.Errorf("%s: no pod security context", p.Name)
			default:
				if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
					t.Errorf("%s: runAsNonRoot is not set", p.Name)
				}
				if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
					t.Errorf("%s: seccomp is %v, want RuntimeDefault", p.Name, sc.SeccompProfile)
				}
			}

			for _, ctr := range p.Spec.Containers {
				csc := ctr.SecurityContext
				if csc == nil {
					t.Errorf("%s/%s: no container security context", p.Name, ctr.Name)
					continue
				}
				if csc.AllowPrivilegeEscalation == nil || *csc.AllowPrivilegeEscalation {
					t.Errorf("%s/%s: privilege escalation is not denied", p.Name, ctr.Name)
				}
				if csc.Capabilities == nil || len(csc.Capabilities.Drop) != 1 || csc.Capabilities.Drop[0] != "ALL" {
					t.Errorf("%s/%s: capabilities are %v, want drop ALL", p.Name, ctr.Name, csc.Capabilities)
				}
				if csc.ReadOnlyRootFilesystem == nil || !*csc.ReadOnlyRootFilesystem {
					t.Errorf("%s/%s: the root filesystem is writable", p.Name, ctr.Name)
				}
				// The mount is half of that setting, not a detail of it: a
				// read-only root with no writable /tmp is a container that boots
				// and then fails the first time anything reaches for os.TempDir.
				// The failure is late, load-dependent, and looks nothing like this
				// setting, so it is asserted here rather than left to be met.
				var tmp bool
				for _, m := range ctr.VolumeMounts {
					if m.MountPath == "/tmp" {
						tmp = true
					}
				}
				if !tmp {
					t.Errorf("%s/%s: root is read-only with no writable /tmp", p.Name, ctr.Name)
				}
			}

			if p.Spec.ServiceAccountName != saName {
				t.Errorf("%s: runs as service account %q, want %q", p.Name, p.Spec.ServiceAccountName, saName)
			}
			for _, v := range p.Spec.Volumes {
				if strings.HasPrefix(v.Name, "kube-api-access") {
					t.Errorf("%s: mounts an API token at %s", p.Name, v.Name)
				}
			}

			if g := p.Spec.TerminationGracePeriodSeconds; g == nil || *g < 35 {
				t.Errorf("%s: grace period %v, want more than the 30s shutdown takes", p.Name, g)
			}

			// atlantis migrates before it binds the health port, so its boot
			// budget has to be larger than its steady-state one. The comparison
			// is between the two budgets rather than against fixed numbers,
			// because the thing that must stay true is the ordering: whatever the
			// liveness settings become, a first boot must have longer than a
			// single stall does.
			if name == nameAtlantis {
				for _, ctr := range p.Spec.Containers {
					sp, lp := ctr.StartupProbe, ctr.LivenessProbe
					if sp == nil {
						t.Errorf("%s/%s: no startup probe, so liveness times the migrations", p.Name, ctr.Name)
						continue
					}
					if lp == nil {
						continue
					}
					boot := sp.InitialDelaySeconds + sp.PeriodSeconds*sp.FailureThreshold
					steady := lp.InitialDelaySeconds + lp.PeriodSeconds*lp.FailureThreshold
					if boot <= steady {
						t.Errorf("%s/%s: boot budget %ds does not exceed the liveness budget %ds",
							p.Name, ctr.Name, boot, steady)
					}
				}
			}
		}
		if seen != 2 {
			t.Fatalf("found %d atlantis/signer pods, want 2", seen)
		}
	})

	// The namespace label is what refuses a pod nothing here wrote — a debug
	// container, a Job, anything applied by hand. Enforcement is the field that
	// rejects; warn and audit only report, so checking `enforce` alone would pass
	// on a namespace that merely complains.
	t.Run("the namespace enforces restricted pod security", func(t *testing.T) {
		var got corev1.Namespace
		if err := c.Get(ctx, types.NamespacedName{Name: k.cfg.Namespace(org)}, &got); err != nil {
			t.Fatal(err)
		}
		for _, label := range []string{psaEnforce, psaAudit, psaWarn} {
			if v := got.Labels[label]; v != psaLevel {
				t.Errorf("%s = %q, want %q", label, v, psaLevel)
			}
		}
	})

	// The security property that cannot be checked from a manifest alone: the
	// authority's private key is not on the atlantis pod's filesystem.
	t.Run("the authority key is not in the atlantis pod", func(t *testing.T) {
		var d appsv1.Deployment
		if err := c.Get(ctx, types.NamespacedName{
			Namespace: k.cfg.Namespace(org), Name: nameAtlantis,
		}, &d); err != nil {
			t.Fatal(err)
		}
		for _, v := range d.Spec.Template.Spec.Volumes {
			if v.Secret != nil && v.Secret.SecretName == secretSignerPKI {
				t.Fatal("the atlantis pod mounts the signer's secret")
			}
		}
	})

	// A second Ensure must not mint a new authority. Proven against a real API
	// server rather than a fake, because server-side apply is where a
	// regression would hide.
	t.Run("re-provisioning keeps the authority", func(t *testing.T) {
		var before corev1.Secret
		if err := c.Get(ctx, types.NamespacedName{
			Namespace: k.cfg.Namespace(org), Name: secretPKI,
		}, &before); err != nil {
			t.Fatal(err)
		}
		if _, err := k.Ensure(ctx, Spec{Org: org}); err != nil {
			t.Fatal(err)
		}
		var after corev1.Secret
		if err := c.Get(ctx, types.NamespacedName{
			Namespace: k.cfg.Namespace(org), Name: secretPKI,
		}, &after); err != nil {
			t.Fatal(err)
		}
		if string(before.Data["ca.crt"]) != string(after.Data["ca.crt"]) {
			t.Fatal("the authority changed on re-provision; every issued caller certificate is now orphaned")
		}
	})
}

// Tenant isolation, proven rather than declared.
//
// The policy objects are applied whether or not the cluster enforces them: an
// API server accepts a NetworkPolicy under any CNI, and kindnet implements
// none. So this asserts the mechanism, not the manifest.
//
// It runs both directions on purpose. A test that only checks the blocked case
// passes just as happily when the probe itself is broken — a typo in the
// address, a pod that never started, an image that is not there. The
// same-namespace probe is the control that says the probe can succeed at all.
func TestK8sTenantsCannotReachEachOthersDatabase(t *testing.T) {
	if os.Getenv("ATLANTIS_TEST_K8S") == "" {
		t.Skip("set ATLANTIS_TEST_K8S to exercise network policy against a real cluster")
	}

	cfg, err := ctrlconfig.GetConfig()
	if err != nil {
		t.Fatalf("no cluster configuration: %v", err)
	}
	scheme, err := NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	c, err := ctrlclient.New(cfg, ctrlclient.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}

	pcfg := testConfig()
	pcfg.ReadyTimeout = 6 * time.Minute
	pcfg.MemcachedAddr = "memcached.atlantis-system.svc.cluster.local:11211"
	k, err := NewKube(pcfg, c, nil)
	if err != nil {
		t.Fatal(err)
	}

	const victim = "victim"
	ctx := context.Background()
	t.Cleanup(func() {
		_ = k.Destroy(context.Background(), victim)
		_ = c.Delete(context.Background(), &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: "tenant-probe"},
		})
	})

	if _, err := k.Ensure(ctx, Spec{Org: victim}); err != nil {
		t.Fatal(err)
	}
	if err := k.WaitReady(ctx, victim); err != nil {
		t.Log(describe(ctx, t, k, victim))
		t.Fatalf("WaitReady: %v", err)
	}

	victimNS := k.cfg.Namespace(victim)
	target := fmt.Sprintf("pg-rw.%s.svc.cluster.local", victimNS)

	// Control: from inside the organisation, the database is reachable. If this
	// fails the isolation result below means nothing.
	if code := probeTCP(ctx, t, c, victimNS, "control", target, 5432); code != 0 {
		t.Fatalf("the control probe could not reach %s from inside %s (exit %d); "+
			"the isolation check below cannot be trusted", target, victimNS, code)
	}

	// The property: another tenant's pod cannot.
	if err := c.Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "tenant-probe"},
	}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	if code := probeTCP(ctx, t, c, "tenant-probe", "intruder", target, 5432); code == 0 {
		t.Fatalf("a pod in another namespace reached %s: tenant databases are not isolated", target)
	}
}

// Every exposed port refuses a foreign pod with no network policy in the way.
//
// This is the test the isolation story rests on, and it is deliberately the
// opposite shape of TestK8sTenantsCannotReachEachOthersDatabase. That one proves
// the policy works. This one deletes the policy and proves the credentials do —
// so that a cloud where the policy means something else is not a security
// question.
//
// It has to be that way because NetworkPolicy is not portable. `ipBlock` covers
// pod traffic under Calico, never covers it under GKE Dataplane V2, and on EKS
// pods take VPC addresses so the pod-CIDR exclusion matches nothing and the rule
// fails open. The same manifest, three meanings. Nothing in the object says
// which one is in force.
//
// So the property worth owning is not "the policy blocks tenants". It is "every
// port is safe with no policy at all". Then the policy is a second layer, and a
// cloud where it is inert costs defence in depth rather than the boundary.
func TestK8sEveryExposedPortRefusesAForeignPodWithNoNetworkPolicy(t *testing.T) {
	if os.Getenv("ATLANTIS_TEST_K8S") == "" {
		t.Skip("set ATLANTIS_TEST_K8S to exercise the exposed ports against a real cluster")
	}

	cfg, err := ctrlconfig.GetConfig()
	if err != nil {
		t.Fatalf("no cluster configuration: %v", err)
	}
	scheme, err := NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	c, err := ctrlclient.New(cfg, ctrlclient.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}

	pcfg := testConfig()
	pcfg.ReadyTimeout = 6 * time.Minute
	pcfg.MemcachedAddr = "memcached.atlantis-system.svc.cluster.local:11211"
	k, err := NewKube(pcfg, c, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Its own organisation, because this test destroys that organisation's
	// network policies. Sharing one with the other tests would leave them
	// passing or failing depending on the order they ran in.
	const org = "unguarded"
	const probeNS = "port-probe"
	ctx := context.Background()
	t.Cleanup(func() {
		_ = k.Destroy(context.Background(), org)
		_ = c.Delete(context.Background(), &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: probeNS},
		})
	})

	if _, err := k.Ensure(ctx, Spec{Org: org}); err != nil {
		t.Fatal(err)
	}
	if err := k.WaitReady(ctx, org); err != nil {
		t.Log(describe(ctx, t, k, org))
		t.Fatalf("WaitReady: %v", err)
	}
	status, err := k.Ensure(ctx, Spec{Org: org})
	if err != nil {
		t.Fatal(err)
	}
	ns := k.cfg.Namespace(org)

	// Delete every policy, by listing rather than by name. Naming `baseline` and
	// `external-access` would still compile and still pass on the day a fourth
	// policy is added, while quietly testing less than it says.
	var policies networkingv1.NetworkPolicyList
	if err := c.List(ctx, &policies, ctrlclient.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	if len(policies.Items) == 0 {
		t.Fatalf("%s has no network policies to remove; provisioning did not apply any, "+
			"so this test would prove nothing about removing them", ns)
	}
	for i := range policies.Items {
		if err := c.Delete(ctx, &policies.Items[i]); err != nil {
			t.Fatalf("delete network policy %s: %v", policies.Items[i].Name, err)
		}
	}
	var left networkingv1.NetworkPolicyList
	if err := c.List(ctx, &left, ctrlclient.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	if len(left.Items) != 0 {
		t.Fatalf("%d network policies survived deletion in %s; the rest of this test "+
			"would credit the certificate for work the policy is still doing", len(left.Items), ns)
	}
	t.Logf("removed %d network policies from %s", len(policies.Items), ns)

	if err := c.Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: probeNS},
	}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}

	atlantisHost := fmt.Sprintf("%s.%s.svc.cluster.local", nameAtlantis, ns)
	signerHost := fmt.Sprintf("%s.%s.svc.cluster.local", nameSigner, ns)

	// The control, and it runs first for two reasons.
	//
	// The obvious one: it proves a pod in another namespace now reaches this
	// organisation, so a refusal below is the certificate and not the network.
	//
	// The one that is easy to miss: it is the only probe here that expects a
	// *successful* HTTPS response. If the probe image could not speak HTTPS at
	// all, every request would come back probeNoReply — which reads as the
	// expected refusal on 9090 and 7070, and both would pass having tested
	// nothing. This failing first is what stops that.
	readyz := fmt.Sprintf("https://%s:%d/readyz", atlantisHost, portHealth)
	if code := probeHTTPS(ctx, t, c, probeNS, "control-readyz", readyz); code != probeGot200 {
		t.Fatalf("inconclusive: the control probe got %d from %s, want %d (200). "+
			"Either the pod never arrived or the probe cannot speak HTTPS; "+
			"in both cases nothing below would mean anything", code, readyz, probeGot200)
	}

	// Port 8081. It answers a client with no certificate and refuses per route,
	// because the kubelet holds no certificate and still has to reach /healthz
	// and /readyz. Both gated routes are checked: requireClientCert guards two,
	// and testing one of them is how the other quietly loses its guard.
	for _, path := range []string{"/metrics", "/status"} {
		url := fmt.Sprintf("https://%s:%d%s", atlantisHost, portHealth, path)
		name := "gated" + strings.ReplaceAll(path, "/", "-")
		switch code := probeHTTPS(ctx, t, c, probeNS, name, url); code {
		case probeGot401:
			// The property.
		case probeGot200:
			t.Errorf("a pod in %s read %s with no client certificate; "+
				"this port is protected by the network policy alone", probeNS, url)
		default:
			t.Errorf("inconclusive: %s returned %d, which is neither 200 nor 401, "+
				"even though the control reached this same host and port", url, code)
		}
	}

	// Ports 9090 and 7070 use RequireAndVerifyClientCert, so TLS refuses before
	// any request is made and there is no status code to read. Both are checked
	// twice, from two places, because neither check is sufficient alone.
	//
	// From a pod: that the port is reachable now that no policy stands in the
	// way. This is the half that says a refusal is not the network.
	//
	// A pod cannot say more than that. busybox reports a refused handshake as
	// `error getting response: Connection reset by peer` — and a server that had
	// *stopped* asking for a certificate would produce the same words, because a
	// gRPC server rejects an HTTP/1.1 request just as abruptly. Asserting "not
	// 200" from a pod would therefore hold whether or not mTLS was still on: a
	// check that cannot fail for the reason it names.
	for _, tc := range []struct {
		what string
		host string
		port int32
	}{
		{"the admin plane", atlantisHost, portGRPC},
		{"the signer", signerHost, portSigner},
	} {
		if code := probeTCP(ctx, t, c, probeNS, fmt.Sprintf("tcp-%d", tc.port), tc.host, tc.port); code != 0 {
			t.Errorf("inconclusive: %s on %s:%d did not accept a connection from %s (exit %d), "+
				"so nothing about that port has been established",
				tc.what, tc.host, tc.port, probeNS, code)
		}
	}

	// The other half, from the test process, where a real TLS client can say
	// what actually happened rather than guess from wget's wording.
	//
	// The host is outside the pod network, so the policy never applied to it and
	// deleting the policy changes nothing here. That is the point: this asks a
	// different question — *what* refuses — while the probes above establish
	// that the network is no longer the thing doing it. Together they say the
	// certificate is carrying the port. Separately neither does.
	for _, tc := range []struct {
		what string
		addr string
	}{
		{"the admin plane", status.Endpoint},
		{"the signer", strings.TrimPrefix(status.SignerAddr, "https://")},
	} {
		assertRefusesWithoutAClientCertificate(t, tc.what, tc.addr)
	}
}

// assertRefusesWithoutAClientCertificate requires that TLS itself — not the
// protocol layered on top of it — turns away a client holding no certificate.
//
// # Why "the connection failed" is not the assertion
//
// The obvious version of this check is "open a connection, try to use it, and
// require an error". It passes on a server that has stopped requiring client
// certificates altogether, and it was measured doing exactly that. With mTLS
// removed from the admin plane, a plain TLS client is admitted, sends an
// HTTP/1.1 request, and gRPC hangs up on it for speaking the wrong protocol:
//
//	mTLS on   →  remote error: tls: certificate required
//	mTLS off  →  EOF
//
// Both are errors. Only the first is this port's boundary doing anything. A
// check that accepted either would have reported a protected port on a build
// where the protection had been deleted — which is the failure this whole test
// exists to rule out, reproduced inside the test itself.
//
// So the error has to name a certificate. EOF, connection reset and timeout all
// fail, and they fail with a message that says why the result is not evidence.
//
// # Why the handshake is not enough on its own
//
// Under TLS 1.3 the client finishes its side before the server has judged it,
// so `tls.Dial` returns a usable connection and the alert arrives on the first
// read. The exchange below is what surfaces it. A TLS 1.2 server refuses during
// the handshake instead, so both paths lead to the same check.
func assertRefusesWithoutAClientCertificate(t *testing.T, what, addr string) {
	t.Helper()

	dialer := &net.Dialer{Timeout: 10 * time.Second}
	conn, err := tls.DialWithDialer(dialer, "tcp", addr,
		&tls.Config{InsecureSkipVerify: true}) //nolint:gosec // the server's identity is not what is under test
	if err == nil {
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

		if _, werr := conn.Write([]byte("GET / HTTP/1.1\r\nHost: probe\r\nConnection: close\r\n\r\n")); werr != nil {
			err = werr
		} else {
			buf := make([]byte, 64)
			n, rerr := conn.Read(buf)
			if rerr == nil {
				t.Errorf("%s at %s answered a client holding no certificate with %d bytes (%q); "+
					"this port does not require a client certificate",
					what, addr, n, strings.TrimSpace(string(buf[:n])))
				return
			}
			err = rerr
		}
	}

	// Observed shapes: "remote error: tls: certificate required" under TLS 1.3,
	// and "remote error: tls: bad certificate" from a 1.2 server rejecting an
	// empty one. Matching the word rather than the whole sentence keeps this
	// from depending on which version was negotiated.
	if !strings.Contains(err.Error(), "certificate") {
		t.Errorf("%s at %s refused with %q, which does not mention a certificate. "+
			"That is what a port with no client-certificate requirement looks like: "+
			"TLS admits the connection and the protocol above it hangs up. "+
			"Nothing here shows the certificate did the refusing", what, addr, err)
	}
}

// probeTCP runs one TCP connect from a pod in ns and returns its exit code.
func probeTCP(ctx context.Context, t *testing.T, c ctrlclient.Client, ns, name, host string, port int32) int32 {
	t.Helper()
	return runProbe(ctx, t, c, ns, name, fmt.Sprintf("nc -z -w 5 %s %d", host, port))
}

// What probeHTTPS reports. The numbers are arbitrary; what matters is that they
// are distinct from the exit codes wget itself uses, so a result is never
// confused with a failure to run.
const (
	probeGot200  int32 = 20
	probeGot401  int32 = 21
	probeNoReply int32 = 22
)

// probeHTTPS makes one unauthenticated HTTPS request from a pod in ns.
//
// The certificate is deliberately not verified. This asks what the *server*
// does with a client that holds no certificate, and verifying the server's own
// certificate would need the organisation's authority distributed to a probe
// that is standing in for an attacker — who would not have it either.
//
// probeNoReply is the answer that needs care. It means the request produced
// neither 200 nor 401: the port was unreachable, or TLS refused the connection,
// or it timed out. Which of those it was cannot be told from here, so every
// caller needs a separate control that establishes the pod arrived at all.
func probeHTTPS(ctx context.Context, t *testing.T, c ctrlclient.Client, ns, name, url string) int32 {
	t.Helper()

	// wget exits 0 only on a 2xx, so the success path needs no parsing. A 401
	// arrives as `wget: server returned error: HTTP/1.1 401 Unauthorized` on
	// stderr, which is why stderr is folded into the captured output.
	//
	// No -S and no temporary file: busybox does not carry GNU wget's -S in every
	// build, and a probe should not assume a writable filesystem it was never
	// promised.
	script := fmt.Sprintf(
		`out=$(wget -q -O /dev/null -T 10 --no-check-certificate %s 2>&1) && exit %d
case "$out" in *401*) exit %d ;; esac
exit %d`, url, probeGot200, probeGot401, probeNoReply)

	return runProbe(ctx, t, c, ns, name, script)
}

// runProbe runs one shell command in a throwaway pod and returns its exit code.
//
// busybox, because it is already loaded into the cluster — a probe that needs
// an image the node cannot pull would fail for reasons that look exactly like
// the policy working.
func runProbe(ctx context.Context, t *testing.T, c ctrlclient.Client, ns, name, script string) int32 {
	t.Helper()

	// The security context is not incidental. Tenant namespaces enforce Pod
	// Security "restricted", so a bare pod is refused at admission — and the
	// refusal surfaces here as `create probe pod: ... violates PodSecurity`,
	// which is a test that cannot run rather than a policy that failed. The uid
	// is named explicitly because busybox declares no USER and therefore defaults
	// to root, which runAsNonRoot rejects.
	//
	// This probe has to satisfy the same policy the workloads do. If that ever
	// becomes hard, that is worth knowing: it means the policy is stricter than
	// the thing it is protecting can tolerate.
	nobody := int64(65534)
	no, yes := false, true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: &no,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   &yes,
				RunAsUser:      &nobody,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:            "probe",
				Image:           "docker.io/library/busybox:1.36",
				ImagePullPolicy: corev1.PullIfNotPresent,
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &no,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				Command: []string{"sh", "-c", script},
			}},
		},
	}
	_ = c.Delete(ctx, pod)
	if err := c.Create(ctx, pod); err != nil {
		t.Fatalf("create probe pod: %v", err)
	}
	t.Cleanup(func() { _ = c.Delete(context.Background(), pod) })

	deadline := time.Now().Add(90 * time.Second)
	for {
		var got corev1.Pod
		if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &got); err != nil {
			t.Fatalf("read probe pod: %v", err)
		}
		for _, cs := range got.Status.ContainerStatuses {
			if cs.State.Terminated != nil {
				return cs.State.Terminated.ExitCode
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("probe pod %s/%s did not finish: %s", ns, name, got.Status.Phase)
		}
		time.Sleep(2 * time.Second)
	}
}

// describe collects enough to tell why an organisation is not ready, because
// the timeout on its own never says.
func describe(ctx context.Context, t *testing.T, k *Kube, org string) string {
	t.Helper()
	ns := k.cfg.Namespace(org)
	out := fmt.Sprintf("namespace %s:\n", ns)

	var pods corev1.PodList
	if err := k.c.List(ctx, &pods, ctrlclient.InNamespace(ns)); err != nil {
		return out + "  (could not list pods: " + err.Error() + ")"
	}
	for _, p := range pods.Items {
		out += fmt.Sprintf("  pod %s: %s\n", p.Name, p.Status.Phase)
		for _, cs := range p.Status.ContainerStatuses {
			out += fmt.Sprintf("    %s ready=%v restarts=%d", cs.Name, cs.Ready, cs.RestartCount)
			if cs.State.Waiting != nil {
				out += fmt.Sprintf(" waiting=%s: %s", cs.State.Waiting.Reason, cs.State.Waiting.Message)
			}
			if cs.LastTerminationState.Terminated != nil {
				out += fmt.Sprintf(" lastExit=%d", cs.LastTerminationState.Terminated.ExitCode)
			}
			out += "\n"
		}
	}
	return out
}
