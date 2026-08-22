package provision

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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
	t.Run("readyz from the host", func(t *testing.T) {
		url := fmt.Sprintf("http://%s/readyz", status.HealthAddr)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", url, err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", url, resp.StatusCode, body)
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
