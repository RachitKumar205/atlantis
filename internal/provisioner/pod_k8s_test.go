package provisioner_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/rachitkumar205/atlantis/internal/cloud/identity"
	"github.com/rachitkumar205/atlantis/internal/cloud/provision"
	"github.com/rachitkumar205/atlantis/internal/cloud/store"
)

// The provisioner running as a pod, under its own ServiceAccount, provisions a
// real organisation.
//
// TestK8sTheProvisionerRoleIsSufficientAndConfined is a statement about RBAC
// alone: it holds on a cluster where the provisioner is not deployed, where the
// image does not build, where the token is never mounted, and where GetConfig
// falls back to a developer's kubeconfig. Each of those leaves the fleet running
// as cluster-admin with a green suite.
//
// This needs a cluster and Cloud's database, being the seam between them: the
// queue row is written here, the namespace appears there, and nothing in this
// test speaks to the pod in between.
//
// CLOUD_PG_URL is the variable the Makefile already hands the provisioner. This
// has to write to the database the pod is polling; an isolated one, as the
// store's own tests use, would be a queue nothing reads.
func TestK8sTheProvisionerPodProvisionsAnOrganisation(t *testing.T) {
	if os.Getenv("ATLANTIS_TEST_K8S") == "" {
		t.Skip("set ATLANTIS_TEST_K8S to exercise the deployed provisioner")
	}
	pgURL := os.Getenv("CLOUD_PG_URL")
	if pgURL == "" {
		t.Skip("set CLOUD_PG_URL to the database the deployed provisioner polls")
	}

	ctx := context.Background()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	db, err := store.New(ctx, pgURL, quiet)
	if err != nil {
		t.Fatalf("open Cloud's database: %v", err)
	}
	// t.Cleanup, not defer. Deferred calls run as the test function returns,
	// which is *before* any t.Cleanup — so `defer db.Close()` closed the pool
	// out from under the row cleanup below, every row survived, and the queue
	// row stayed `ready`. The next run then saw StateReady on the first poll and
	// passed in under a second without the provisioner touching it.
	//
	// Registered first so it runs last: cleanups are LIFO.
	t.Cleanup(db.Close)

	c := clusterClient(t)
	systemNS := envOr("PROVISIONER_SYSTEM_NAMESPACE", "atlantis-system")

	// Before anything else: is there a provisioner to test? Without this the
	// failure is a timeout on a state that was never going to change, which
	// reads like a broken provisioner rather than an absent one.
	assertProvisionerIsRunning(ctx, t, c, systemNS)

	// A name no other test uses. The K8s tests take itest, victim, unguarded,
	// scoped and port-probe; a collision would destroy an organisation another
	// test is halfway through.
	const org = "deployed"
	const ownerEmail = "deployed-probe@example.test"

	// Cleanup runs first in registration order terms — declared before the rows
	// exist so that a failure between here and the insert still tidies up.
	//
	// The database rows matter more than they look. CreateOrgWithOwner inserts
	// the queue row ON CONFLICT DO NOTHING, so a leftover row still marked
	// `ready` would make the next run observe StateReady immediately and pass
	// without the provisioner having done anything at all.
	t.Cleanup(func() {
		bg := context.Background()
		if k := destroyer(c); k != nil {
			_ = k.Destroy(bg, org)
		}
		for _, sql := range []string{
			`DELETE FROM cloud.org_provisioning WHERE org = $1`,
			`DELETE FROM cloud.memberships WHERE org = $1`,
			`DELETE FROM cloud.orgs WHERE name = $1`,
		} {
			if _, err := db.Pool().Exec(bg, sql, org); err != nil {
				t.Logf("cleanup %q: %v", sql, err)
			}
		}
		if _, err := db.Pool().Exec(bg,
			`DELETE FROM cloud.users WHERE email = $1`, ownerEmail); err != nil {
			t.Logf("cleanup the owner: %v", err)
		}
	})

	owner, err := db.CreateUser(ctx, ownerEmail, "Deployed Probe", "", nil)
	if err != nil {
		t.Fatalf("create the owner: %v", err)
	}
	if err := db.CreateOrgWithOwner(ctx, org, "Deployed", owner.ID, identity.RoleAdmin); err != nil {
		t.Fatalf("enqueue %s: %v", org, err)
	}
	t.Logf("queued %s; nothing in this test tells the provisioner about it", org)

	// The poll interval is 10s and provisioning takes about a minute, so this is
	// generous rather than tight: a slow node should not read as a broken
	// service account.
	deadline := time.Now().Add(6 * time.Minute)
	var last store.ProvisioningState
	for {
		p, err := db.ProvisioningFor(ctx, org)
		if err != nil {
			t.Fatalf("read the queue row: %v", err)
		}
		if p.State != last {
			t.Logf("%s: %s (attempts %d)", org, p.State, p.Attempts)
			last = p.State
		}
		if p.State == store.StateReady {
			break
		}
		// One failure is not fatal — the worker retries, and a first attempt can
		// lose a race with CloudNativePG. Three means it is not a race.
		if p.State == store.StateFailed && p.Attempts >= 3 {
			t.Fatalf("%s failed %d times, last error: %s\n\n"+
				"A Forbidden here is a verb the ClusterRole is missing that the "+
				"impersonation test does not reach", org, p.Attempts, p.LastError)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never became ready (last state %q, %d attempts, last error %q)",
				org, p.State, p.Attempts, p.LastError)
		}
		time.Sleep(5 * time.Second)
	}

	// The queue saying ready is the provisioner's own claim about its work, so
	// it is checked against the cluster rather than believed. A bug that marked
	// rows ready without building anything would satisfy everything above.
	orgNS := "org-" + org
	var got corev1.Namespace
	if err := c.Get(ctx, types.NamespacedName{Name: orgNS}, &got); err != nil {
		t.Fatalf("the queue says %s is ready but %s does not exist: %v", org, orgNS, err)
	}
	for _, name := range []string{"atlantis", "signer"} {
		var pods corev1.PodList
		if err := c.List(ctx, &pods, ctrlclient.InNamespace(orgNS),
			ctrlclient.MatchingLabels{"app.kubernetes.io/name": name}); err != nil {
			t.Fatal(err)
		}
		if len(pods.Items) == 0 {
			t.Errorf("%s reached ready with no %s pod", orgNS, name)
		}
	}
}

// assertProvisionerIsRunning fails with the reason rather than letting the
// caller time out against an empty namespace.
func assertProvisionerIsRunning(ctx context.Context, t *testing.T, c ctrlclient.Client, ns string) {
	t.Helper()

	var pods corev1.PodList
	if err := c.List(ctx, &pods, ctrlclient.InNamespace(ns),
		ctrlclient.MatchingLabels{"app.kubernetes.io/name": "atlantis-provisioner"}); err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) == 0 {
		t.Fatalf("no provisioner pod in %s — run `make dev-k8s-load`, which builds "+
			"the image and applies the Deployment", ns)
	}

	p := pods.Items[0]
	if p.Status.Phase != corev1.PodRunning {
		t.Fatalf("the provisioner pod is %s, not Running", p.Status.Phase)
	}
	// The identity is the entire point, so it is asserted rather than assumed
	// from the manifest. A pod that fell back to `default` would still provision
	// — using whatever that account can do — and every other assertion in this
	// file would pass while describing the wrong subject.
	if p.Spec.ServiceAccountName != "atlantis-provisioner" {
		t.Fatalf("the provisioner runs as service account %q, not atlantis-provisioner; "+
			"nothing this test proves would be about the intended identity",
			p.Spec.ServiceAccountName)
	}
}

func clusterClient(t *testing.T) ctrlclient.Client {
	t.Helper()

	cfg, err := ctrlconfig.GetConfig()
	if err != nil {
		t.Fatalf("no cluster configuration: %v", err)
	}
	scheme, err := provision.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	c, err := ctrlclient.New(cfg, ctrlclient.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// destroyer builds a Kube for cleanup only, using the test's own credentials
// rather than the provisioner's. Cleanup is not the thing under test, and a
// tidy-up refused by RBAC would leave a namespace behind for every later run.
//
// The image and host values are required by the config but unread by Destroy,
// which deletes the namespace and nothing else.
func destroyer(c ctrlclient.Client) *provision.Kube {
	k, err := provision.NewKube(provision.Config{
		ExternalHost:  "cleanup.invalid",
		ServerImage:   "cleanup",
		SignerImage:   "cleanup",
		PostgresImage: "cleanup",
		MemcachedAddr: "cleanup:11211",
	}, c, nil)
	if err != nil {
		return nil
	}
	return k
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
