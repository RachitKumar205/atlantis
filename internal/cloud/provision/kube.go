package provision

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"time"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/rachitkumar205/atlantis/internal/cloud/provision/certs"
)

// fieldOwner identifies this process to Kubernetes' server-side apply.
//
// Fixed, and it matters that it is: every apply claims ownership of the fields
// it sets, so a second provisioner using a different name would fight this one
// field by field rather than converging with it.
const fieldOwner = ctrlclient.FieldOwner("atlantis-provisioner")

// Object names inside an organisation's namespace. Fixed rather than derived
// from the organisation, because the namespace already carries that and a name
// like org-acme/atlantis-acme reads as though the two could disagree.
const (
	nameAtlantis = "atlantis"
	nameSigner   = "signer"
	namePostgres = "pg"

	// secretPKI is the source of truth for this organisation's certificates.
	// Mounted by nothing; it exists so a repeated Ensure can reuse the
	// authority it already minted instead of orphaning every certificate
	// issued under it.
	secretPKI = "atlantis-pki"

	// The three interfaces derived from it.
	secretAtlantisTLS  = "atlantis-tls"
	secretSignerPKI    = "signer-pki"
	secretConsoleCreds = "console-client"
)

// Ports. The health listener is plain HTTP and separate from gRPC by design;
// see cmd/server/health.go.
const (
	portGRPC   int32 = 9090
	portHealth int32 = 8081
	portSigner int32 = 7070
)

// Kube provisions organisations into a Kubernetes cluster.
type Kube struct {
	cfg Config
	c   ctrlclient.Client
	log *slog.Logger
}

// NewScheme builds the type registry this package needs.
//
// CloudNativePG's types are registered alongside the built-in ones so a single
// typed client handles both, rather than the usual split of a typed client for
// core objects and a dynamic one for the custom resource — which would mean two
// clients, two error shapes, and field names as string literals on the half
// that matters most.
func NewScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return nil, fmt.Errorf("register built-in types: %w", err)
	}
	if err := cnpgv1.AddToScheme(s); err != nil {
		return nil, fmt.Errorf("register CloudNativePG types: %w", err)
	}
	return s, nil
}

// NewKube returns a Target backed by the given client.
//
// The client is passed in rather than built here so tests can supply a fake and
// so the caller owns how it authenticates — a kubeconfig locally, a service
// account in a cluster.
func NewKube(cfg Config, c ctrlclient.Client, log *slog.Logger) (*Kube, error) {
	cfg = cfg.withDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if c == nil {
		return nil, errors.New("provision: a Kubernetes client is required")
	}
	if log == nil {
		log = slog.Default()
	}
	return &Kube{cfg: cfg, c: c, log: log}, nil
}

// Ensure converges one organisation towards running.
//
// The order is a dependency order, not a preference. The namespace has to exist
// before anything in it; the policy goes in early so there is never a window
// where a tenant's pods are reachable; the certificates precede the workloads
// that mount them; Postgres precedes atlantis because atlantis applies the
// migrations and needs somewhere to apply them to.
//
// The signer is deliberately *not* sequenced after atlantis. Its startup does
// only a database ping — the caller lookup happens at issue time — so it can
// converge in parallel, crash-looping harmlessly until Postgres answers.
func (k *Kube) Ensure(ctx context.Context, spec Spec) (Status, error) {
	if spec.Org == "" {
		return Status{}, errors.New("provision: Spec.Org is required")
	}
	ns := k.cfg.Namespace(spec.Org)
	log := k.log.With("org", spec.Org, "namespace", ns)

	if err := k.apply(ctx, k.namespace(ns, spec.Org)); err != nil {
		return Status{}, fmt.Errorf("namespace: %w", err)
	}
	for _, p := range k.networkPolicies(ns) {
		if err := k.apply(ctx, p); err != nil {
			return Status{}, fmt.Errorf("network policy %s: %w", p.GetName(), err)
		}
	}
	// Before the workloads: a pod naming a service account that does not exist
	// is admitted and then never starts, and the event says only "error looking
	// up service account", which reads like a permissions problem.
	if err := k.apply(ctx, k.serviceAccount(ns)); err != nil {
		return Status{}, fmt.Errorf("service account: %w", err)
	}

	bundle, err := k.ensureCerts(ctx, ns, spec.Org)
	if err != nil {
		return Status{}, fmt.Errorf("certificates: %w", err)
	}
	for _, s := range k.derivedSecrets(ns, bundle) {
		if err := k.apply(ctx, s); err != nil {
			return Status{}, fmt.Errorf("secret %s: %w", s.GetName(), err)
		}
	}

	if err := k.apply(ctx, k.postgres(ns)); err != nil {
		return Status{}, fmt.Errorf("postgres: %w", err)
	}
	for _, o := range k.workloads(ns) {
		if err := k.apply(ctx, o); err != nil {
			return Status{}, fmt.Errorf("%s: %w", o.GetName(), err)
		}
	}

	status, err := k.addresses(ctx, ns, bundle)
	if err != nil {
		return Status{}, err
	}

	// Readiness last, and reported rather than returned as an error: an
	// organisation that is still initialising is not a failed organisation, and
	// the difference decides whether a caller retries or gives up.
	ready, err := k.ready(ctx, ns)
	if err != nil {
		return status, err
	}
	// Ready means a caller could actually use this organisation, so it needs an
	// address as well as a running pod. The two can lag each other by a moment
	// and the answer must be the conjunction, not the workload half alone.
	status.Ready = ready && status.Endpoint != "" && status.HealthAddr != "" && status.SignerAddr != ""
	// "converged", not "provisioned", and it reports the conjunction above
	// rather than the workload half.
	//
	// Both halves of that were wrong and both mislead in the same direction.
	// Ensure runs on every call, including the ones that applied everything and
	// are still waiting on a pod or a NodePort, so a success-sounding message
	// here is how a watcher concludes an organisation is serving when it is
	// not — which is exactly what happened the first time cmd/provisioner was
	// run against a real cluster. And logging `ready` rather than
	// `status.Ready` reports something the caller never sees: they differ
	// precisely when the pods are up and no address has been allocated yet,
	// which is the ordinary state of a first call.
	log.Info("converged", "ready", status.Ready, "endpoint", status.Endpoint)
	return status, nil
}

// ensureCerts returns this organisation's certificates, minting them only if
// there are none.
//
// # Why "does the Secret exist" is not the test
//
// It is the obvious implementation and it converges on a permanently broken
// organisation. An interrupted apply, a hand-edit, or a Secret written by an
// older version of this code all leave something that exists and cannot be
// used — and a presence check treats that as "already done", so the signer
// crash-loops forever while every retry reports success. The whole point of
// being Ensure-shaped is that a retry repairs things, and a presence check is
// the one shape that cannot.
//
// So the material is parsed and checked before it is trusted, and anything
// present-but-unusable is an error rather than a reuse. That is deliberately
// not self-healing: regenerating would mint a new authority and silently orphan
// every caller certificate already issued under the old one, which is worse
// than stopping and saying so.
func (k *Kube) ensureCerts(ctx context.Context, ns, org string) (*certs.Bundle, error) {
	var existing corev1.Secret
	err := k.c.Get(ctx, types.NamespacedName{Namespace: ns, Name: secretPKI}, &existing)
	switch {
	case err == nil:
		bundle, verr := bundleFromSecret(&existing)
		if verr != nil {
			return nil, fmt.Errorf(
				"the stored certificates for %q are unusable (%w).\n\n"+
					"They are not regenerated automatically: a fresh authority would "+
					"invalidate every caller certificate already issued for this "+
					"organisation. Delete secret %s/%s to re-provision from scratch, "+
					"accepting that every caller must enrol again",
				org, verr, ns, secretPKI)
		}
		return bundle, nil
	case !apierrors.IsNotFound(err):
		return nil, err
	}

	bundle, err := certs.Generate(certs.Options{
		Org: org,
		// Both names the server is dialled by. internal/console/client.go
		// leaves ServerName unset on purpose, so the leaf must match whatever
		// address was used — and there are two: the in-cluster service and the
		// external host a caller reaches.
		ServerDNSNames: []string{
			k.cfg.ExternalHost,
			fmt.Sprintf("%s.%s.svc.cluster.local", nameAtlantis, ns),
			fmt.Sprintf("%s.%s.svc", nameAtlantis, ns),
			nameAtlantis,
		},
		SignerDNSNames: []string{
			k.cfg.ExternalHost,
			fmt.Sprintf("%s.%s.svc.cluster.local", nameSigner, ns),
			fmt.Sprintf("%s.%s.svc", nameSigner, ns),
			nameSigner,
		},
	})
	if err != nil {
		return nil, err
	}
	if err := k.apply(ctx, k.pkiSecret(ns, bundle)); err != nil {
		return nil, err
	}
	return bundle, nil
}

// bundleFromSecret reads a stored bundle back and refuses anything it cannot
// prove is usable.
//
// Every check here corresponds to a way the material can be present and wrong:
// a missing key from a truncated write, a mismatched pair from a partial
// rotation, a leaf that no longer chains because somebody replaced one half.
func bundleFromSecret(s *corev1.Secret) (*certs.Bundle, error) {
	get := func(k string) []byte { return s.Data[k] }

	b := &certs.Bundle{
		CA:           certs.Authority{CertPEM: get("ca.crt"), KeyPEM: get("ca.key")},
		SignerCA:     certs.Authority{CertPEM: get("signer-ca.crt"), KeyPEM: get("signer-ca.key")},
		Server:       certs.Leaf{CertPEM: get("server.crt"), KeyPEM: get("server.key")},
		Console:      certs.Leaf{CertPEM: get("console.crt"), KeyPEM: get("console.key")},
		SignerServer: certs.Leaf{CertPEM: get("signer-server.crt"), KeyPEM: get("signer-server.key")},
		SignerClient: certs.Leaf{CertPEM: get("signer-client.crt"), KeyPEM: get("signer-client.key")},
	}

	pairs := []struct {
		what string
		cert []byte
		key  []byte
	}{
		{"issuing CA", b.CA.CertPEM, b.CA.KeyPEM},
		{"signer CA", b.SignerCA.CertPEM, b.SignerCA.KeyPEM},
		{"atlantis server", b.Server.CertPEM, b.Server.KeyPEM},
		{"console client", b.Console.CertPEM, b.Console.KeyPEM},
		{"signer server", b.SignerServer.CertPEM, b.SignerServer.KeyPEM},
		{"signer client", b.SignerClient.CertPEM, b.SignerClient.KeyPEM},
	}
	for _, p := range pairs {
		if len(p.cert) == 0 || len(p.key) == 0 {
			return nil, fmt.Errorf("%s is missing", p.what)
		}
		if _, err := tls.X509KeyPair(p.cert, p.key); err != nil {
			return nil, fmt.Errorf("%s: certificate and key are not a pair: %w", p.what, err)
		}
	}

	// Chain, not just pair. A cert/key pair that no longer chains to the stored
	// authority produces a handshake failure at a caller rather than here.
	for _, c := range []struct {
		what  string
		leaf  []byte
		root  []byte
		usage x509.ExtKeyUsage
	}{
		{"atlantis server", b.Server.CertPEM, b.CA.CertPEM, x509.ExtKeyUsageServerAuth},
		{"console client", b.Console.CertPEM, b.CA.CertPEM, x509.ExtKeyUsageClientAuth},
		{"signer server", b.SignerServer.CertPEM, b.SignerCA.CertPEM, x509.ExtKeyUsageServerAuth},
		{"signer client", b.SignerClient.CertPEM, b.SignerCA.CertPEM, x509.ExtKeyUsageClientAuth},
	} {
		if err := verifyChain(c.leaf, c.root, c.usage); err != nil {
			return nil, fmt.Errorf("%s does not chain to its authority: %w", c.what, err)
		}
	}
	return b, nil
}

func verifyChain(leafPEM, rootPEM []byte, usage x509.ExtKeyUsage) error {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(rootPEM) {
		return errors.New("the authority has no usable certificate")
	}
	leaf, err := parseFirstCert(leafPEM)
	if err != nil {
		return err
	}
	_, err = leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{usage}})
	return err
}

func parseFirstCert(certPEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	return x509.ParseCertificate(block.Bytes)
}

// ready reports whether every workload is serving.
//
// A timeout is not consulted here: this asks once and answers. Waiting is the
// caller's decision, because a provisioner draining a work queue and an
// operator running a command want different patience.
func (k *Kube) ready(ctx context.Context, ns string) (bool, error) {
	var cluster cnpgv1.Cluster
	if err := k.c.Get(ctx, types.NamespacedName{Namespace: ns, Name: namePostgres}, &cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if cluster.Status.Phase != cnpgv1.PhaseHealthy {
		return false, nil
	}

	for _, name := range []string{nameAtlantis, nameSigner} {
		var d appsv1.Deployment
		if err := k.c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &d); err != nil {
			if apierrors.IsNotFound(err) {
				return false, nil
			}
			return false, err
		}
		if d.Status.ReadyReplicas < 1 {
			return false, nil
		}
	}
	return true, nil
}

// WaitReady polls until the organisation is serving or the configured timeout
// elapses.
//
// A timeout here is retryable and says so: the objects are applied, so calling
// Ensure again finds them and carries on. Reporting it as terminal would fail
// an organisation permanently because initdb was slow once.
func (k *Kube) WaitReady(ctx context.Context, org string) error {
	ns := k.cfg.Namespace(org)
	deadline := time.Now().Add(k.cfg.ReadyTimeout)
	for {
		ready, err := k.ready(ctx, ns)
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("provision: %s was not ready within %s "+
				"(this is retryable — the objects are applied)", org, k.cfg.ReadyTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// Destroy removes the namespace and everything in it.
//
// Deleting the namespace is the whole operation: every object this package
// creates is namespaced, so there is nothing to clean up outside it. Whether
// the Postgres volume survives is the storage class's reclaim policy to decide,
// not this function's.
// Exists reports whether this organisation is still present in the cluster.
//
// The namespace stands in for the whole organisation because everything Ensure
// creates lives inside it and goes with it: if the namespace is gone, so are
// the certificates, the database and both workloads, and there is nothing left
// to converge towards.
//
// A namespace being deleted right now counts as absent. It cannot be reused —
// Kubernetes refuses to create objects in a terminating namespace — so treating
// it as present would report an organisation as healthy for as long as its
// teardown took, which is exactly when somebody is looking.
func (k *Kube) Exists(ctx context.Context, org string) (bool, error) {
	if org == "" {
		return false, errors.New("provision: an organisation name is required")
	}
	var ns corev1.Namespace
	err := k.c.Get(ctx, ctrlclient.ObjectKey{Name: k.cfg.Namespace(org)}, &ns)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return ns.DeletionTimestamp == nil, nil
}

func (k *Kube) Destroy(ctx context.Context, org string) error {
	if org == "" {
		return errors.New("provision: an organisation name is required")
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: k.cfg.Namespace(org)}}
	if err := k.c.Delete(ctx, ns); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	k.log.Info("destroyed", "org", org, "namespace", k.cfg.Namespace(org))
	return nil
}

// apply is server-side apply: create-or-update in one call, with no read first
// and therefore no window between deciding and acting.
//
// This uses Patch with the apply patch type rather than the newer
// Client.Apply, and the deprecation notice on it is not an oversight.
// Client.Apply takes a runtime.ApplyConfiguration, which is an interface
// satisfied only by generated apply-configuration types — they carry an
// IsApplyConfiguration marker method. Kubernetes ships those for its own API
// groups; CloudNativePG ships none for Cluster. Since this package applies core
// objects and a custom resource through one code path, the typed-object patch
// is the only form that covers both, and splitting it into two mechanisms to
// avoid a deprecation warning would make the CRD half the less-tested one.
//
// Revisit if CloudNativePG starts generating apply configurations.
//
//nolint:staticcheck // see above: Client.Apply cannot express a CRD without generated apply configurations
func (k *Kube) apply(ctx context.Context, obj ctrlclient.Object) error {
	return k.c.Patch(ctx, obj, ctrlclient.Apply, fieldOwner, ctrlclient.ForceOwnership)
}

// addresses reads back the ports Kubernetes allocated and assembles what
// registration needs.
//
// Read back rather than requested: a NodePort chosen by us is a NodePort that
// collides with somebody else's eventually. Ports never appear in a certificate,
// so learning them after minting is not an ordering problem.
func (k *Kube) addresses(ctx context.Context, ns string, b *certs.Bundle) (Status, error) {
	grpcPort, err := k.nodePort(ctx, ns, nameAtlantis, "grpc")
	if err != nil {
		return Status{}, err
	}
	healthPort, err := k.nodePort(ctx, ns, nameAtlantis, "health")
	if err != nil {
		return Status{}, err
	}
	signerPort, err := k.nodePort(ctx, ns, nameSigner, "https")
	if err != nil {
		return Status{}, err
	}

	// An address is only reported once its port exists. A half-filled Status
	// invites a caller to register "acme:0" and discover the problem at a
	// handshake days later, so the fields stay empty until they are true — and
	// Ensure reports Ready false, which is the signal to call again.
	addr := func(port int32) string {
		if port == 0 {
			return ""
		}
		return fmt.Sprintf("%s:%d", k.cfg.ExternalHost, port)
	}
	signerURL := ""
	if signerPort != 0 {
		signerURL = fmt.Sprintf("https://%s", addr(signerPort))
	}
	return Status{
		Endpoint:       addr(grpcPort),
		PublicEndpoint: addr(grpcPort),
		HealthAddr:     addr(healthPort),
		SignerAddr:     signerURL,

		CAPEM:          b.CA.CertPEM,
		ConsoleCertPEM: b.Console.CertPEM,
		ConsoleKeyPEM:  b.Console.KeyPEM,

		SignerCAPEM:         b.SignerCA.CertPEM,
		SignerClientCertPEM: b.SignerClient.CertPEM,
		SignerClientKeyPEM:  b.SignerClient.KeyPEM,
	}, nil
}

// nodePort reads back an allocated port.
//
// A port of zero with no error means Kubernetes has not assigned one yet, which
// is a normal moment in the life of a Service and not a failure. Reporting it as
// an error would make a freshly applied organisation look broken for the second
// or two before the allocation lands — and a caller that retries on failure
// would be retrying something already correct. Only a Service that is missing
// altogether, or has no port by that name, is wrong.
func (k *Kube) nodePort(ctx context.Context, ns, name, portName string) (int32, error) {
	var svc corev1.Service
	if err := k.c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &svc); err != nil {
		if apierrors.IsNotFound(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read service %s: %w", name, err)
	}
	for _, p := range svc.Spec.Ports {
		if p.Name == portName {
			return p.NodePort, nil
		}
	}
	return 0, fmt.Errorf("service %s has no port named %q", name, portName)
}

var _ Target = (*Kube)(nil)
