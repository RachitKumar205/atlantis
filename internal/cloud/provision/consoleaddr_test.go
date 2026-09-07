package provision

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// Which address the console is given for an organisation.
//
// Endpoint and PublicEndpoint differ once the console is inside the cluster and
// callers are not. If both are the external host and a NodePort, an in-cluster
// console asks cluster DNS for a name that only resolves outside and every page
// fails with
//
//	dns: A record lookup error: lookup atl-dev.test on 10.96.0.10:53: server misbehaving
//
// which reads as a broken organisation and is really a question about where the
// console is running.

// svcWithNodePorts builds the two Services addresses() reads.
//
// The NodePorts matter: an address is withheld until one is allocated, so a
// fixture without them would make every assertion below pass against a Status
// full of empty strings.
func svcWithNodePorts(ns string) []ctrlclient.Object {
	return []ctrlclient.Object{
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: nameAtlantis, Namespace: ns},
			Spec: corev1.ServiceSpec{
				Type: corev1.ServiceTypeNodePort,
				Ports: []corev1.ServicePort{
					{Name: "grpc", Port: portGRPC, NodePort: 31111},
					{Name: "health", Port: portHealth, NodePort: 32222},
				},
			},
		},
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: nameSigner, Namespace: ns},
			Spec: corev1.ServiceSpec{
				Type: corev1.ServiceTypeNodePort,
				Ports: []corev1.ServicePort{
					{Name: "https", Port: portSigner, NodePort: 33333},
				},
			},
		},
	}
}

func statusFor(t *testing.T, inCluster bool) Status {
	t.Helper()
	const ns = "org-acme"
	k := newTestKube(t, svcWithNodePorts(ns)...)
	k.cfg.ConsoleInCluster = inCluster

	b, err := k.ensureCerts(context.Background(), ns, "acme")
	if err != nil {
		t.Fatalf("ensureCerts: %v", err)
	}
	st, err := k.addresses(context.Background(), ns, "acme", b)
	if err != nil {
		t.Fatalf("addresses: %v", err)
	}
	return st
}

// With an organisation domain, each organisation is named on its own
// subdomain in the addresses a caller is given and in the certificate it
// verifies.
func TestAnOrganisationDomainNamesEachOrganisation(t *testing.T) {
	const ns = "org-acme"
	k := newTestKube(t, svcWithNodePorts(ns)...)
	k.cfg.ExternalHost = ""
	k.cfg.OrgDomain = "example.dev"

	b, err := k.ensureCerts(context.Background(), ns, "acme")
	if err != nil {
		t.Fatalf("ensureCerts: %v", err)
	}
	st, err := k.addresses(context.Background(), ns, "acme", b)
	if err != nil {
		t.Fatalf("addresses: %v", err)
	}
	if st.PublicEndpoint != "acme.example.dev:31111" {
		t.Errorf("PublicEndpoint is %q, want acme.example.dev:31111", st.PublicEndpoint)
	}
	if !strings.HasPrefix(st.SignerAddr, "https://acme.example.dev:") {
		t.Errorf("SignerAddr is %q, want the organisation's own name", st.SignerAddr)
	}
	for _, c := range []struct {
		what string
		pem  []byte
	}{{"server", b.Server.CertPEM}, {"signer", b.SignerServer.CertPEM}} {
		blk, _ := pem.Decode(c.pem)
		if blk == nil {
			t.Fatalf("%s: no PEM block", c.what)
		}
		cert, perr := x509.ParseCertificate(blk.Bytes)
		if perr != nil {
			t.Fatal(perr)
		}
		if err := cert.VerifyHostname("acme.example.dev"); err != nil {
			t.Errorf("the %s certificate does not name acme.example.dev: %v", c.what, err)
		}
	}
}

// Both names, or neither, is refused.
func TestOneHostSettingIsRequired(t *testing.T) {
	base := Config{ServerImage: "s", SignerImage: "g", PostgresImage: "p", MemcachedAddr: "m:11211"}
	if err := base.validate(); err == nil || !strings.Contains(err.Error(), "ExternalHost or OrgDomain") {
		t.Errorf("neither host: err = %v", err)
	}
	both := base
	both.ExternalHost, both.OrgDomain = "atl-dev.test", "example.dev"
	if err := both.validate(); err == nil || !strings.Contains(err.Error(), "both set") {
		t.Errorf("both hosts: err = %v", err)
	}
}

// An in-cluster console is given Service names; a caller is still given the node.
func TestAnInClusterConsoleGetsServiceNames(t *testing.T) {
	st := statusFor(t, true)

	for _, c := range []struct{ what, got, want string }{
		{"Endpoint", st.Endpoint, "atlantis.org-acme.svc.cluster.local:9090"},
		{"HealthAddr", st.HealthAddr, "atlantis.org-acme.svc.cluster.local:8081"},
		{"SignerAddr", st.SignerAddr, "https://signer.org-acme.svc.cluster.local:7070"},
	} {
		if c.got != c.want {
			t.Errorf("%s is %q, want %q — an in-cluster console cannot resolve the "+
				"external host", c.what, c.got, c.want)
		}
	}

	// The caller's address is unchanged. Callers are outside the cluster and a
	// Service name means nothing to them.
	if st.PublicEndpoint != "atl-dev.test:31111" {
		t.Errorf("PublicEndpoint is %q, want the node address", st.PublicEndpoint)
	}
	// The two must differ.
	if st.Endpoint == st.PublicEndpoint {
		t.Error("Endpoint and PublicEndpoint are identical; the console and a " +
			"caller are being told to dial the same address from different networks")
	}
}

// `make dev-console-app` runs a console outside the cluster. Handing out
// Service names unconditionally breaks it.
func TestAConsoleOutsideTheClusterGetsTheNodeAddress(t *testing.T) {
	st := statusFor(t, false)

	if st.Endpoint != "atl-dev.test:31111" {
		t.Errorf("Endpoint is %q, want the node address for an external console", st.Endpoint)
	}
	if st.Endpoint != st.PublicEndpoint {
		t.Errorf("Endpoint %q and PublicEndpoint %q differ for a console that is "+
			"outside the cluster, where both dial the same way",
			st.Endpoint, st.PublicEndpoint)
	}
	if !strings.HasPrefix(st.SignerAddr, "https://atl-dev.test:") {
		t.Errorf("SignerAddr is %q, want the node address", st.SignerAddr)
	}
}

// The console leaves tls.Config.ServerName unset, so the leaf has to match
// whatever address was dialled. A Service name absent from the SANs fails the
// handshake with a certificate error rather than a DNS one, on the first page
// load.
func TestTheConsolesAddressIsCoveredByTheServerCertificate(t *testing.T) {
	const ns = "org-acme"
	k := newTestKube(t, svcWithNodePorts(ns)...)
	k.cfg.ConsoleInCluster = true

	b, err := k.ensureCerts(context.Background(), ns, "acme")
	if err != nil {
		t.Fatal(err)
	}
	st, err := k.addresses(context.Background(), ns, "acme", b)
	if err != nil {
		t.Fatal(err)
	}

	leaf := func(pemBytes []byte) *x509.Certificate {
		t.Helper()
		blk, _ := pem.Decode(pemBytes)
		if blk == nil {
			t.Fatal("no PEM block")
		}
		c, perr := x509.ParseCertificate(blk.Bytes)
		if perr != nil {
			t.Fatal(perr)
		}
		return c
	}
	hostOf := func(addr string) string {
		addr = strings.TrimPrefix(addr, "https://")
		if i := strings.LastIndex(addr, ":"); i >= 0 {
			return addr[:i]
		}
		return addr
	}

	if err := leaf(b.Server.CertPEM).VerifyHostname(hostOf(st.Endpoint)); err != nil {
		t.Errorf("the atlantis certificate does not cover %q, the address the "+
			"console is told to dial: %v", hostOf(st.Endpoint), err)
	}
	if err := leaf(b.SignerServer.CertPEM).VerifyHostname(hostOf(st.SignerAddr)); err != nil {
		t.Errorf("the signer certificate does not cover %q: %v", hostOf(st.SignerAddr), err)
	}
	// The caller's address too, from the same leaf — one certificate serves both
	// networks, which is why moving the console needs no reissue.
	if err := leaf(b.Server.CertPEM).VerifyHostname(hostOf(st.PublicEndpoint)); err != nil {
		t.Errorf("the atlantis certificate does not cover %q, the caller's "+
			"address: %v", hostOf(st.PublicEndpoint), err)
	}
}

// Readiness is the conjunction of these fields being non-empty. A Service name
// resolves as soon as the Service exists, so letting the in-cluster form fill in
// early would report an organisation ready while no caller could reach it.
func TestNoAddressIsReportedBeforeItsPortIsAllocated(t *testing.T) {
	const ns = "org-acme"
	unallocated := []ctrlclient.Object{
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: nameAtlantis, Namespace: ns},
			Spec: corev1.ServiceSpec{
				Type: corev1.ServiceTypeNodePort,
				Ports: []corev1.ServicePort{
					{Name: "grpc", Port: portGRPC},
					{Name: "health", Port: portHealth},
				},
			},
		},
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: nameSigner, Namespace: ns},
			Spec: corev1.ServiceSpec{
				Type:  corev1.ServiceTypeNodePort,
				Ports: []corev1.ServicePort{{Name: "https", Port: portSigner}},
			},
		},
	}

	for _, inCluster := range []bool{true, false} {
		k := newTestKube(t, unallocated...)
		k.cfg.ConsoleInCluster = inCluster
		b, err := k.ensureCerts(context.Background(), ns, "acme")
		if err != nil {
			t.Fatal(err)
		}
		st, err := k.addresses(context.Background(), ns, "acme", b)
		if err != nil {
			t.Fatal(err)
		}
		if st.Endpoint != "" || st.HealthAddr != "" || st.SignerAddr != "" {
			t.Errorf("ConsoleInCluster=%v reported addresses before any NodePort was "+
				"allocated: %q %q %q — an organisation would look ready with nothing "+
				"reachable", inCluster, st.Endpoint, st.HealthAddr, st.SignerAddr)
		}
	}
}

// The external-access rule admits "everything except the pod network", which is
// how a caller is distinguished from another tenant's pod. An in-cluster
// console's traffic comes from inside that CIDR, so that rule excludes it — and
// a NetworkPolicy drops rather than rejects, so the page loads forever and then
// reports
//
//	dial tcp 10.104.217.247:9090: i/o timeout
//
// after DNS resolved.
func TestTheConsoleIsAllowedThroughTheTenantIsolationRule(t *testing.T) {
	k := newTestKube(t)
	var console *networkingv1.NetworkPolicy
	for _, o := range k.networkPolicies("org-acme") {
		if p, ok := o.(*networkingv1.NetworkPolicy); ok && p.Name == "console-access" {
			console = p
		}
	}
	if console == nil {
		t.Fatal("no console-access policy; an in-cluster console is refused by " +
			"external-access, which excludes the pod network")
	}

	if len(console.Spec.Ingress) != 1 || len(console.Spec.Ingress[0].From) != 1 {
		t.Fatalf("want exactly one ingress rule with one peer, got %+v", console.Spec.Ingress)
	}
	peer := console.Spec.Ingress[0].From[0]

	// Both selectors in ONE peer. Split across two they become an OR, which
	// admits every pod in the control-plane namespace — Cloud, the provisioner,
	// a debugging shell — to a tenant's admin port.
	if peer.NamespaceSelector == nil {
		t.Error("the peer has no namespace selector, so any namespace with a pod " +
			"labelled atlantis-console reaches this organisation")
	}
	if peer.PodSelector == nil {
		t.Error("the peer has no pod selector, so everything in the control-plane " +
			"namespace reaches this organisation's admin port")
	}
	if peer.IPBlock != nil {
		t.Error("the peer carries an IPBlock, which would widen it beyond the console")
	}
	if got := peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]; got != "atlantis-system" {
		t.Errorf("namespace selector matches %q, want atlantis-system", got)
	}
	if got := peer.PodSelector.MatchLabels["app.kubernetes.io/name"]; got != "atlantis-console" {
		t.Errorf("pod selector matches %q, want atlantis-console", got)
	}

	// Every port the console uses: admin, health and the signer it enrols
	// through. A missing one fails only on the page that needs it.
	ports := map[int32]bool{}
	for _, p := range console.Spec.Ingress[0].Ports {
		if p.Port != nil {
			ports[p.Port.IntVal] = true
		}
	}
	for _, want := range []int32{portGRPC, portHealth, portSigner} {
		if !ports[want] {
			t.Errorf("the console is not admitted on port %d", want)
		}
	}
}

// The console policy is additive. If external-access ever stopped excluding the
// pod network, every tenant would reach every other tenant's admin port and the
// console policy above would look like the thing that permitted it.
func TestTenantIsolationSurvivesTheConsoleAllowance(t *testing.T) {
	k := newTestKube(t)
	var external *networkingv1.NetworkPolicy
	for _, o := range k.networkPolicies("org-acme") {
		if p, ok := o.(*networkingv1.NetworkPolicy); ok && p.Name == "external-access" {
			external = p
		}
	}
	if external == nil {
		t.Fatal("no external-access policy")
	}
	block := external.Spec.Ingress[0].From[0].IPBlock
	if block == nil {
		t.Fatal("external-access no longer uses an IPBlock; tenant isolation " +
			"depended on excluding the pod network from it")
	}
	if len(block.Except) == 0 {
		t.Fatal("external-access no longer excludes the pod network, so one " +
			"tenant's pod can reach another tenant's admin port")
	}
}
