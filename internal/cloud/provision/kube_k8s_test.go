package provision

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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

// probeTCP runs one TCP connect from a pod in ns and returns its exit code.
//
// busybox, because it is already loaded into the cluster — a probe that needs
// an image the node cannot pull would fail for reasons that look exactly like
// the policy working.
func probeTCP(ctx context.Context, t *testing.T, c ctrlclient.Client, ns, name, host string, port int32) int32 {
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
				Command: []string{
					"sh", "-c",
					fmt.Sprintf("nc -z -w 5 %s %d", host, port),
				},
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
