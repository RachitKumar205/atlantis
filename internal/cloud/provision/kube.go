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
	"github.com/rachitkumar205/atlantis/internal/secrets"
)

// fieldOwner identifies this process to Kubernetes' server-side apply.
//
// Fixed: every apply claims ownership of the fields it sets, so a second
// provisioner using a different name would fight this one field by field rather
// than converging with it.
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
	secretDataKey      = "atlantis-data-key"
)

// Ports. atlantis serves health on its own listener, separate from gRPC and
// also over TLS; see cmd/server/health.go.
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
// CloudNativePG's types are registered alongside the built-in ones so one typed
// client handles both, rather than a typed client for core objects and a
// dynamic one for the custom resource, where field names are string literals.
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
// A dependency order: namespace before anything in it, network policy early so
// no window exists where a tenant's pods are reachable, certificates before the
// workloads that mount them, Postgres before atlantis, which migrates it.
//
// The signer is not sequenced after atlantis; its startup is a database ping.
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
	// Minted before the workloads, because the atlantis Deployment mounts it.
	// The value is not read here: the pod reads the Secret.
	if _, err := k.ensureDataKey(ctx, ns, spec.Org); err != nil {
		return Status{}, fmt.Errorf("data keyset: %w", err)
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
	// organisation still initialising is not a failed one, and the difference
	// decides whether a caller retries.
	ready, err := k.ready(ctx, ns)
	if err != nil {
		return status, err
	}
	// Ready needs an address as well as a running pod; the two lag each other
	// by a moment.
	status.Ready = ready && status.Endpoint != "" && status.HealthAddr != "" && status.SignerAddr != ""
	// "converged", not "provisioned": Ensure runs on every call, including ones
	// still waiting on a pod or a NodePort. status.Ready, not the local `ready`
	// — the two differ when the pods are up and no address is allocated.
	log.Info("converged", "ready", status.Ready, "endpoint", status.Endpoint)
	return status, nil
}

// ensureCerts returns this organisation's certificates, minting them only if
// there are none.
//
// The material is parsed, not merely found: an interrupted apply leaves a
// Secret that exists and cannot be used, which a presence check treats as done.
//
// Present-but-unusable is an error, not a reuse. Regenerating would mint a new
// authority and orphan every caller certificate issued under the old one.
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
		// leaves ServerName unset, so the leaf must match whatever
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

// ensureDataKey returns this organisation's data keyset, minting one the first
// time.
//
// It seals the managed database's DSN in atlantis.managed_database. Never
// regenerated: a new keyset cannot decrypt what the old one sealed, so the
// organisation would keep running against a DSN nothing can read and the only
// symptom would be plans against the wrong database.
//
// Refused rather than replaced when unreadable, for the same reason
// ensureCerts refuses a broken authority.
func (k *Kube) ensureDataKey(ctx context.Context, ns, org string) (string, error) {
	var existing corev1.Secret
	err := k.c.Get(ctx, types.NamespacedName{Namespace: ns, Name: secretDataKey}, &existing)
	switch {
	case err == nil:
		keyset := string(existing.Data["keyset"])
		if keyset == "" {
			return "", fmt.Errorf(
				"the stored data keyset for %q is empty.\n\n"+
					"It is not regenerated automatically: a new keyset cannot decrypt "+
					"the managed-database connection string the old one sealed. Delete "+
					"secret %s/%s to mint a fresh one, accepting that the managed "+
					"database must be named again",
				org, ns, secretDataKey)
		}
		return keyset, nil
	case !apierrors.IsNotFound(err):
		return "", err
	}

	keyset, err := secrets.NewKeyset()
	if err != nil {
		return "", fmt.Errorf("generate data keyset for %q: %w", org, err)
	}
	if err := k.apply(ctx, &corev1.Secret{
		TypeMeta:   typeMeta("v1", "Secret"),
		ObjectMeta: k.meta(ns, secretDataKey, "data-key"),
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{"keyset": []byte(keyset)},
	}); err != nil {
		return "", err
	}
	return keyset, nil
}

// bundleFromSecret reads a stored bundle back and refuses anything it cannot
// prove is usable.
//
// Each check corresponds to a way the material is present and wrong: a missing
// key from a truncated write, a mismatched pair from a partial rotation, a leaf
// that stopped chaining because one half was replaced.
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

	// Chain, not just pair. A cert/key pair that does not chain to the stored
	// authority fails the handshake at a caller rather than here.
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
// Asks once. Waiting is the caller's decision: a provisioner draining a work
// queue and an operator running a command want different patience.
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
// Ensure again finds them and carries on.
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

// Exists reports whether this organisation is still present in the cluster.
//
// The namespace stands in for the organisation: everything Ensure creates lives
// inside it and goes with it.
//
// A terminating namespace counts as absent. Kubernetes refuses to create
// objects in one, so treating it as present would report the organisation
// healthy for the whole teardown.
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

// Destroy removes the namespace and everything in it. Every object this package
// creates is namespaced, so nothing is left outside it. Whether the Postgres
// volume survives is the storage class's reclaim policy.
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

// apply is server-side apply: create-or-update in one call, with no read first.
//
// Patch with the apply patch type, not Client.Apply, whose
// runtime.ApplyConfiguration is satisfied only by generated types, and
// CloudNativePG ships none for Cluster. This package applies core objects and a
// custom resource through one path. Revisit if that changes.
//
//nolint:staticcheck // see above: Client.Apply cannot express a CRD without generated apply configurations
func (k *Kube) apply(ctx context.Context, obj ctrlclient.Object) error {
	return k.c.Patch(ctx, obj, ctrlclient.Apply, fieldOwner, ctrlclient.ForceOwnership)
}

// addresses reads back the ports Kubernetes allocated and assembles what
// registration needs.
//
// Read back rather than requested: a NodePort chosen here eventually collides
// with another organisation's. Ports never appear in a certificate, so learning
// them after minting is not an ordering problem.
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

	// An address is only reported once its port exists: a half-filled Status
	// invites a caller to register "acme:0" and discover it at a handshake days
	// later. Ensure reports Ready false meanwhile.
	addr := func(port int32) string {
		if port == 0 {
			return ""
		}
		return fmt.Sprintf("%s:%d", k.cfg.ExternalHost, port)
	}

	// What the console dials, which is not always what a caller dials.
	//
	// Gated on the same NodePort allocation as the external form, though a
	// Service name resolves as soon as the Service exists. Readiness is the
	// conjunction of these fields being non-empty, so filling the in-cluster
	// address in first would report an organisation ready while no caller could
	// reach it.
	consoleAddr := func(service string, nodePort, servicePort int32) string {
		if nodePort == 0 {
			return ""
		}
		if !k.cfg.ConsoleInCluster {
			return addr(nodePort)
		}
		return fmt.Sprintf("%s.%s.svc.cluster.local:%d", service, ns, servicePort)
	}

	endpoint := consoleAddr(nameAtlantis, grpcPort, portGRPC)
	healthAddr := consoleAddr(nameAtlantis, healthPort, portHealth)
	signerURL := ""
	if s := consoleAddr(nameSigner, signerPort, portSigner); s != "" {
		signerURL = "https://" + s
	}
	return Status{
		Endpoint:       endpoint,
		PublicEndpoint: addr(grpcPort),
		HealthAddr:     healthAddr,
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
// is a normal moment in the life of a Service. Only a Service that is missing
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

// RotateConsoleCredentials reissues the two certificates the console presents
// to this organisation, keeping both authorities. It rotates when the leaf
// expires within renewWithin, or when force is set, and returns a Status either
// way so the caller can re-register regardless.
//
// The expiry lives in the stored bundle, so deciding outside would mean two
// reads that can disagree. Nothing restarts afterwards: neither
// secretConsoleCreds nor the PKI Secret is mounted by any pod.
func (k *Kube) RotateConsoleCredentials(
	ctx context.Context, org string, renewWithin time.Duration, force bool,
) (ConsoleRotation, error) {
	ns := k.cfg.Namespace(org)
	log := k.log.With("org", org, "namespace", ns)

	var stored corev1.Secret
	if err := k.c.Get(ctx, types.NamespacedName{Namespace: ns, Name: secretPKI}, &stored); err != nil {
		if apierrors.IsNotFound(err) {
			return ConsoleRotation{}, fmt.Errorf(
				"%s has no certificates to rotate: secret %s/%s does not exist",
				org, ns, secretPKI)
		}
		return ConsoleRotation{}, err
	}
	// The same parse-and-check ensureCerts does: rotating from a bundle that
	// cannot be verified would write a second unusable one over it.
	bundle, err := bundleFromSecret(&stored)
	if err != nil {
		return ConsoleRotation{}, fmt.Errorf("the stored certificates for %q are unusable: %w", org, err)
	}

	expires, err := consoleLeafExpiry(bundle)
	if err != nil {
		return ConsoleRotation{}, fmt.Errorf("%s: %w", org, err)
	}
	due, why := consoleRotationDue(expires, renewWithin, time.Now())
	if force {
		due, why = true, "an operator asked"
	}
	if !due {
		// Still current. The addresses are read anyway, so a caller that
		// re-registers on every pass gets a Status describing what is deployed,
		// and ExpiresAt without a second read.
		status, aerr := k.addresses(ctx, ns, bundle)
		return ConsoleRotation{Status: status, ExpiresAt: expires}, aerr
	}

	// From here on every failure carries the expiry of the certificate still in
	// place, which is what the organisation runs on until the rotation lands.
	stillInPlace := ConsoleRotation{ExpiresAt: expires}

	if err := certs.ReissueConsoleLeaves(bundle, time.Time{}); err != nil {
		return stillInPlace, fmt.Errorf("%s: %w", org, err)
	}
	// Read back from the bundle rather than computed as now+ClientLifetime:
	// deriving it here would be a second copy of the lifetime rule.
	renewed, err := consoleLeafExpiry(bundle)
	if err != nil {
		return stillInPlace, fmt.Errorf("%s: %w", org, err)
	}

	// The PKI Secret first, because it is the one ensureCerts reads back. Stop
	// between these two writes and the next pass reconstructs from a bundle
	// that already holds the new leaves and rewrites the derived copy; the
	// other order leaves the reconstruction source holding credentials nothing
	// had registered.
	if err := k.apply(ctx, k.pkiSecret(ns, bundle)); err != nil {
		return stillInPlace, fmt.Errorf("write the rotated certificates for %q: %w", org, err)
	}
	for _, s := range k.derivedSecrets(ns, bundle) {
		if err := k.apply(ctx, s); err != nil {
			return stillInPlace, fmt.Errorf("secret %s: %w", s.GetName(), err)
		}
	}

	status, err := k.addresses(ctx, ns, bundle)
	if err != nil {
		return stillInPlace, err
	}
	log.Info("rotated the console's credentials",
		"why", why,
		"previous_expiry", expires.Format(time.RFC3339),
		"expires", renewed.Format(time.RFC3339), "forced", force)
	return ConsoleRotation{
		Status: status, Rotated: true,
		ExpiresAt: renewed, PreviousExpiresAt: expires,
	}, nil
}

// policyDriftTolerance separates "issued under a longer policy" from clock
// jitter.
//
// A leaf minted a moment ago has almost exactly ClientLifetime left, so the
// comparison below needs slack or every fresh certificate looks over-long and
// rotates itself on the next pass, reissuing the fleet every reconcile
// interval. An hour is far larger than any skew and far smaller than any policy
// change.
const policyDriftTolerance = time.Hour

// consoleRotationDue decides whether a credential should be replaced, and says
// why. The reason is returned rather than logged; this has no logger.
//
// Two triggers: an approaching expiry, and a certificate outliving the current
// policy. On expiry alone, shortening ClientLifetime would apply to nothing,
// since a ten-year certificate is never within ten days of expiring. The second
// fires once per organisation, the replacement carrying the new lifetime.
func consoleRotationDue(expires time.Time, renewWithin time.Duration, now time.Time) (bool, string) {
	left := expires.Sub(now)
	if left <= renewWithin {
		return true, "the certificate is inside the renewal window"
	}
	if left > certs.ClientLifetime+policyDriftTolerance {
		return true, "the certificate outlives the current lifetime policy"
	}
	return false, ""
}

// consoleLeafExpiry reports when the console's certificate for atlantis runs
// out.
//
// Only that one, though the rotation replaces two: mintConsoleLeaves mints both
// together with the same lifetime, so they expire together.
func consoleLeafExpiry(b *certs.Bundle) (time.Time, error) {
	blk, _ := pem.Decode(b.Console.CertPEM)
	if blk == nil {
		return time.Time{}, errors.New("the console certificate is not PEM")
	}
	leaf, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse the console certificate: %w", err)
	}
	return leaf.NotAfter, nil
}
