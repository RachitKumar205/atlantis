// Package provision turns "an organisation exists" into "an organisation has a
// running atlantis".
//
// One organisation gets one Kubernetes namespace holding its own Postgres, its
// own atlantis, its own signer, and its own pair of certificate authorities.
// Nothing is shared between organisations except the node they land on and the
// operators that manage them.
//
// Nothing here writes to a database. Ensure reports what it built and the
// caller decides what to record: the Kubernetes objects converge by
// re-applying, while the database rows are the state machine that decides
// whether re-applying is wanted at all.
//
// Provisioning spans a cluster and two databases and takes most of a minute, so
// it will be interrupted. Every step converges rather than creates: applying
// twice is the normal case and the second run is a no-op. Certificate
// generation is the exception, because fresh keys on every call are what
// certs.Generate promises; ensureCerts reads what is already there and reuses
// it.
package provision

import (
	"context"
	"fmt"
	"time"
)

// Spec describes one organisation to provision.
//
// Everything else that shapes the result — images, storage class, resource
// requests, the host organisations are reached at — lives in Config, so two
// organisations cannot be given different storage classes.
type Spec struct {
	// Org is the organisation name. It becomes a namespace and appears in the
	// certificate authorities' common names, so it is held to the same shape
	// Cloud holds it to: a DNS label.
	Org string
}

// Status is what Ensure reports back, and is exactly what registration needs.
//
// The certificate material is returned rather than left for the caller to find
// in the cluster: that it comes out of a Secret is particular to the Kubernetes
// target.
type Status struct {
	// Ready is true only when every component is serving. Ready false with no
	// error means provisioning is still converging and the call should be
	// repeated.
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

// ConsoleRotation is what one pass over an organisation's console credentials
// found, and what it did about it.
type ConsoleRotation struct {
	// Status describes the organisation as deployed, whether or not anything
	// rotated, so a caller can re-register from it either way.
	Status Status

	// Rotated reports whether new certificates were actually issued. False with
	// no error means the existing ones were still comfortably in date.
	Rotated bool

	// ExpiresAt is when the certificate now stored for this organisation runs
	// out: the new one when a rotation happened, otherwise the one that was
	// already there.
	//
	// Reported alongside an error whenever the certificate was read before the
	// failure, so a rotation that keeps failing still shows the credential
	// counting down.
	//
	// Zero means the call failed before reading any certificate. A caller
	// watching the fleet must treat zero as unknown rather than expired: the
	// cluster is not answering, which is what the rotation failure count is for.
	ExpiresAt time.Time

	// PreviousExpiresAt is when the superseded certificate runs out. Zero unless
	// this call rotated.
	//
	// Registration is the caller's step, and until it succeeds the console is
	// still presenting the old certificate, so the old expiry is what describes
	// the organisation's exposure.
	PreviousExpiresAt time.Time
}

// Target is somewhere an organisation can be provisioned.
//
// One implementation today, against Kubernetes. The seam keeps "decide what
// this organisation needs" testable without a cluster.
type Target interface {
	// Ensure converges the organisation towards running and reports where it
	// got to. Safe to call repeatedly.
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

	// ExternalHost is the name callers reach organisations at. The console
	// reaches them at Status.Endpoint instead; see ConsoleInCluster.
	//
	// A name, never an address. The local cluster's node IP changes on every
	// recreate while its name does not, so an address here would bake a
	// certificate that stops verifying the next time the cluster is rebuilt.
	//
	// Exactly one of ExternalHost and OrgDomain is set.
	ExternalHost string

	// OrgDomain gives each organisation its own name, <org>.<OrgDomain>, in
	// its certificates and endpoints. A wildcard DNS record under the domain
	// points every one at the same load balancer; the port still tells them
	// apart. An organisation name is a DNS label by construction (see
	// identity.ValidateOrgName), and the platform's own names under the domain
	// are reserved there.
	//
	// The name is written into an organisation's certificates once, at first
	// provisioning; ensureCerts reuses a stored bundle as it is. Switching
	// between ExternalHost and OrgDomain therefore re-provisions nothing on
	// its own: an existing organisation keeps its old name until its PKI
	// Secret is deleted, and every caller then enrols again.
	OrgDomain string

	// ConsoleInCluster reports whether the console runs beside these
	// organisations rather than outside the cluster. It decides
	// Status.Endpoint, the address the console dials, as against
	// Status.PublicEndpoint, the address a caller dials.
	//
	// A deployment fact this package cannot work out. An in-cluster console
	// dialling ExternalHost asks cluster DNS to resolve a name that exists only
	// outside, and gets "server misbehaving" from the resolver.
	//
	// ensureCerts puts the in-cluster service names in the leaf's SANs
	// alongside ExternalHost, so switching this needs no reissue.
	ConsoleInCluster bool

	// StorageClass for the Postgres volume. Empty uses the cluster default,
	// which is whatever that cluster's operator chose.
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
	// wrong address here means the pod never becomes Ready. cmd/server defaults
	// to localhost:11211, which in a pod is wrong and not fatal at startup.
	MemcachedAddr string

	// PodCIDR is the cluster's pod network.
	//
	// The network policy is written as "everything except this range", which
	// lets the console and callers reach an organisation from outside the
	// cluster while no pod inside it can.
	//
	// A wrong value is not symmetric. Too narrow and the console is blocked,
	// which is loud. Too wide or stale and tenant pods fall outside the
	// exception and are admitted, with every manifest still saying otherwise.
	//
	// It has to match CALICO_IPV4POOL_CIDR in deploy/k8s-dev.sh, and nothing
	// enforces that: the cluster CIDR is not readable from the API, since a
	// Node's spec.podCIDR is that node's slice rather than the cluster's range.
	// TestK8sTenantsCannotReachEachOthersDatabase probes the property instead.
	PodCIDR string

	// OperatorNamespace is where CloudNativePG runs.
	//
	// It needs an explicit allow to reach each Postgres instance. Omitting it
	// makes the policy block the controller that creates the thing the policy
	// protects, and the cluster never becomes ready.
	OperatorNamespace string

	// ControlPlaneNamespace is where the console runs.
	//
	// The organisation's network policy allows that namespace's console pod to
	// reach the ports an organisation serves on. Without it an in-cluster
	// console is refused by the tenant isolation rule, which is written as
	// "everything except the pod network" and so excludes anything running
	// inside the cluster.
	//
	// The allow is narrow: this namespace AND the console's own pod label, not
	// the namespace alone. Anything else running beside the console gets
	// nothing, and another tenant's pod is still refused.
	ControlPlaneNamespace string

	// PostgresInstances is 1 for development. Three is the floor for a cluster
	// with an availability promise: CloudNativePG hard-restarts a
	// single-instance cluster on every operator upgrade and blocks node drains.
	PostgresInstances int32

	// PostgresStorage is a Kubernetes quantity, e.g. "1Gi".
	PostgresStorage string

	// ReadyTimeout bounds each wait. Exceeding it is retryable: the objects are
	// applied, so the next Ensure finds them and carries on. A first provision
	// pulls no images (they are side-loaded) but does run initdb.
	ReadyTimeout time.Duration
}

// Namespace is where this organisation's objects live.
func (c Config) Namespace(org string) string { return c.NamespacePrefix + org }

// withDefaults fills in what a caller did not set.
//
// Images and the host have no default: guessing an image reference produces
// a pod that cannot start, and guessing a hostname produces certificates that
// cannot verify. validate refuses both.
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
	if c.ControlPlaneNamespace == "" {
		c.ControlPlaneNamespace = "atlantis-system"
	}
	return c
}

// Host is the name callers reach org at.
func (c Config) Host(org string) string {
	if c.OrgDomain != "" {
		return org + "." + c.OrgDomain
	}
	return c.ExternalHost
}

func (c Config) validate() error {
	if c.ExternalHost != "" && c.OrgDomain != "" {
		return fmt.Errorf("provision: ExternalHost (%q) and OrgDomain (%q) are both set; "+
			"an organisation is reached at one name or the other", c.ExternalHost, c.OrgDomain)
	}
	missing := []string{}
	if c.ExternalHost == "" && c.OrgDomain == "" {
		missing = append(missing, "ExternalHost or OrgDomain")
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
		// Named rather than defaulted: cmd/server's default is wrong in a pod.
		missing = append(missing, "MemcachedAddr")
	}
	if len(missing) > 0 {
		return fmt.Errorf("provision: config is incomplete: %v", missing)
	}
	return nil
}
