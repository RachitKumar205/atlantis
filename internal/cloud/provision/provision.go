// Package provision turns "an organisation exists" into "an organisation has a
// running atlantis".
//
// One organisation gets one Kubernetes namespace holding its own Postgres, its
// own atlantis, its own signer, and its own pair of certificate authorities.
// Nothing is shared between organisations except the node they happen to land
// on and the operators that manage them.
//
// # What this package does not do
//
// It writes nothing to any database. Provisioning an organisation and recording
// that it was provisioned are separate concerns with separate failure modes:
// the Kubernetes objects are converged by re-applying them, while the database
// rows are the state machine that decides whether re-applying is even wanted.
// Ensure reports what it built and the caller decides what to remember.
//
// # Everything here is Ensure-shaped
//
// Provisioning spans a cluster and two databases and takes the better part of a
// minute, so it will be interrupted — by a restart, a timeout, a full disk. Every
// step is therefore written to converge rather than to create: applying twice is
// the normal case, not the exception, and the second run must be a no-op rather
// than a duplicate or an error.
//
// The one place that is not naturally idempotent is certificate generation,
// because fresh keys on every call are what certs.Generate promises. That is
// handled by reading what is already there and reusing it — see ensureCerts,
// which also explains why "does the Secret exist" is not the test.
package provision

import (
	"context"
	"fmt"
	"time"
)

// Spec describes one organisation to provision.
//
// Deliberately thin. Everything else that shapes the result — images, storage
// class, resource requests, the host organisations are reached at — lives in
// Config, because it is a property of the deployment rather than of the
// organisation, and because a caller should not be able to give two
// organisations different storage classes by accident.
type Spec struct {
	// Org is the organisation name. It becomes a namespace and appears in the
	// certificate authorities' common names, so it is held to the same shape
	// Cloud holds it to: a DNS label.
	Org string
}

// Status is what Ensure reports back, and is exactly what registration needs.
//
// The certificate material is here rather than left in the cluster for the
// caller to find because the caller should not need to know how this package
// stores anything. That it happens to come out of a Secret is an implementation
// detail of the Kubernetes target.
type Status struct {
	// Ready is true only when every component is serving. A Status with
	// Ready false and no error means provisioning is still converging and the
	// call should be repeated — not that it failed.
	Ready bool

	// Endpoint is host:port for the admin gRPC service, as the console dials
	// it. PublicEndpoint is the same service as a caller dials it. They are
	// equal in a single-network deployment and differ once the console is
	// inside the cluster and callers are not, which is why console.orgs keeps
	// them apart.
	Endpoint       string
	PublicEndpoint string

	// HealthAddr is host:port for the plain-HTTP health listener. The console's
	// Health page reaches this directly rather than over gRPC, so it is a
	// separate address and not derivable from Endpoint.
	HealthAddr string

	// SignerAddr is the https:// base URL of this organisation's signer.
	SignerAddr string

	// The credentials the console needs to reach this organisation.
	//
	// CAPEM is the root its atlantis trusts, and is also what the shared
	// enrolment listener must add to its client-CA pool before any caller in
	// this organisation can renew a certificate.
	CAPEM          []byte
	ConsoleCertPEM []byte
	ConsoleKeyPEM  []byte

	// The credentials the console needs to reach this organisation's signer.
	// A second, independent authority — see internal/cloud/provision/certs.
	SignerCAPEM         []byte
	SignerClientCertPEM []byte
	SignerClientKeyPEM  []byte
}

// Target is somewhere an organisation can be provisioned.
//
// One implementation today, against Kubernetes. The interface exists because
// the alternative is a package whose every function takes a Kubernetes client,
// which makes the seam between "decide what this organisation needs" and "make
// it exist here" impossible to test without a cluster.
type Target interface {
	// Ensure converges the organisation towards running and reports where it
	// got to. It is safe to call repeatedly and expected to be.
	Ensure(ctx context.Context, spec Spec) (Status, error)

	// Destroy removes everything Ensure created, including the data.
	Destroy(ctx context.Context, org string) error
}

// Config is the deployment-shaped half of provisioning: the values that differ
// between a laptop and a production cluster, and nothing that differs between
// one organisation and another.
type Config struct {
	// NamespacePrefix is prepended to the organisation name. "org-" gives
	// namespaces like org-acme, which are easy to select and hard to confuse
	// with anything the platform runs.
	NamespacePrefix string

	// ExternalHost is the name callers and the console reach organisations at.
	//
	// A name, never an address. The local cluster's node IP changes on every
	// recreate while its name does not, so an address here would bake a
	// certificate that stops verifying the next time the cluster is rebuilt.
	ExternalHost string

	// StorageClass for the Postgres volume. Empty uses the cluster default,
	// which is fine locally and is not something to rely on in a cluster where
	// somebody else chose the default.
	StorageClass string

	// Images. PostgresImage must carry an Apache-2 TimescaleDB and pgvector,
	// and must be tagged with something CloudNativePG can read as a Postgres
	// version — see Dockerfile.pg.
	ServerImage   string
	SignerImage   string
	PostgresImage string

	// PullPolicy is IfNotPresent for a cluster whose images are side-loaded,
	// which is every cluster this runs against today. Always would fail on a
	// node with no route to a registry.
	PullPolicy string

	// MemcachedAddr is host:port of the shared cache.
	//
	// Not optional in practice: atlantis's readiness probe performs a real
	// cache operation and reports 503 on anything but a hit or a miss, so a
	// wrong address here means the pod never becomes Ready. The default in
	// cmd/server is localhost:11211, which in a pod is always wrong and never
	// fatal at startup — the failure surfaces as a permanent readiness
	// failure instead.
	MemcachedAddr string

	// PodCIDR is the cluster's pod network.
	//
	// The network policy is written as "everything except this range", which is
	// what lets the console and callers reach an organisation from outside the
	// cluster while no pod inside it can. A wrong value here fails safe in one
	// direction and open in the other: too narrow and the console is blocked,
	// too wide and tenant pods are admitted. It cannot be derived from the API,
	// so it is configuration.
	PodCIDR string

	// OperatorNamespace is where CloudNativePG runs.
	//
	// It needs an explicit allow to reach each Postgres instance, and omitting
	// it produces the failure that is hardest to attribute: the policy blocks
	// the controller that creates the thing the policy is protecting, and the
	// cluster simply never becomes ready.
	OperatorNamespace string

	// PostgresInstances is 1 for development. Three is the floor once anything
	// is promised to anybody: CloudNativePG hard-restarts a single-instance
	// cluster on every operator upgrade and blocks node drains.
	PostgresInstances int32

	// PostgresStorage is a Kubernetes quantity, e.g. "1Gi".
	PostgresStorage string

	// ReadyTimeout bounds each wait. Exceeding it is retryable rather than
	// terminal — the objects are applied, so the next Ensure finds them and
	// carries on. A first provision pulls no images (they are side-loaded) but
	// does run initdb, which is the slow part.
	ReadyTimeout time.Duration
}

// Namespace is where this organisation's objects live.
func (c Config) Namespace(org string) string { return c.NamespacePrefix + org }

// withDefaults fills in what a caller did not set.
//
// Images and ExternalHost have no sensible default and are required: guessing
// an image reference produces a pod that cannot start, and guessing a hostname
// produces certificates that cannot verify. Both fail loudly in validate.
func (c Config) withDefaults() Config {
	if c.NamespacePrefix == "" {
		c.NamespacePrefix = "org-"
	}
	if c.PullPolicy == "" {
		c.PullPolicy = "IfNotPresent"
	}
	if c.PostgresInstances == 0 {
		c.PostgresInstances = 1
	}
	if c.PostgresStorage == "" {
		c.PostgresStorage = "1Gi"
	}
	if c.ReadyTimeout == 0 {
		c.ReadyTimeout = 5 * time.Minute
	}
	if c.PodCIDR == "" {
		// kubeadm's default, which is what both kind and `container k8s` use.
		c.PodCIDR = "10.244.0.0/16"
	}
	if c.OperatorNamespace == "" {
		c.OperatorNamespace = "cnpg-system"
	}
	return c
}

func (c Config) validate() error {
	missing := []string{}
	if c.ExternalHost == "" {
		missing = append(missing, "ExternalHost")
	}
	if c.ServerImage == "" {
		missing = append(missing, "ServerImage")
	}
	if c.SignerImage == "" {
		missing = append(missing, "SignerImage")
	}
	if c.PostgresImage == "" {
		missing = append(missing, "PostgresImage")
	}
	if c.MemcachedAddr == "" {
		// Named explicitly rather than defaulted, because the default that
		// exists in cmd/server is the one value guaranteed to be wrong here.
		missing = append(missing, "MemcachedAddr")
	}
	if len(missing) > 0 {
		return fmt.Errorf("provision: config is incomplete: %v", missing)
	}
	return nil
}
