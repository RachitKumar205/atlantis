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
// # The failure this exists to stop
//
// Status has always kept Endpoint and PublicEndpoint apart, and its comment says
// they "differ once the console is inside the cluster and callers are not".
// Nothing made them differ: both were the external host and a NodePort. That was
// correct while the console ran on somebody's machine, and became wrong the day
// it moved into the cluster — an in-cluster console asked cluster DNS for a name
// that only resolves outside and every page failed with
//
//	dns: A record lookup error: lookup atl-dev.test on 10.96.0.10:53: server misbehaving
//
// which reads as a broken organisation and is really a question about where the
// console happens to be running.

// svcWithNodePorts builds the two Services addresses() reads.
//
// The NodePorts matter: an address is deliberately withheld until one is
// allocated, and a fixture without them would make every assertion below pass
// against a Status full of empty strings.
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
	st, err := k.addresses(context.Background(), ns, b)
	if err != nil {
		t.Fatalf("addresses: %v", err)
	}
	return st
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
	// And the two must actually differ, which is the whole point. Equal values
	// are how this went unnoticed.
	if st.Endpoint == st.PublicEndpoint {
		t.Error("Endpoint and PublicEndpoint are identical; the console and a " +
			"caller are being told to dial the same address from different networks")
	}
}

// A console outside the cluster still gets the node address.
//
// `make dev-console-app` runs one. Switching this on unconditionally would have
// fixed the deployed console by breaking the one on the developer's machine.
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

// The address the console is handed must be one the certificate covers.
//
// This is what makes the switch safe without reissuing anything, and it is not
// obvious: the console leaves tls.Config.ServerName unset on purpose, so the
// leaf has to match whatever address was dialled. A Service name absent from the
// SANs would fail the handshake with a certificate error rather than a DNS one —
// harder to trace, and only at the moment somebody opens a page.
func TestTheConsolesAddressIsCoveredByTheServerCertificate(t *testing.T) {
	const ns = "org-acme"
	k := newTestKube(t, svcWithNodePorts(ns)...)
	k.cfg.ConsoleInCluster = true

	b, err := k.ensureCerts(context.Background(), ns, "acme")
	if err != nil {
		t.Fatal(err)
	}
	st, err := k.addresses(context.Background(), ns, b)
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

// No address is reported before its NodePort exists, in either mode.
//
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
		st, err := k.addresses(context.Background(), ns, b)
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

// The console is allowed through the tenant isolation rule, and nothing else is.
//
// # Why this needs its own policy
//
// The external-access rule admits "everything except the pod network", which was
// how a caller was distinguished from another tenant's pod while the console was
// also outside. Once the console moved into the cluster its traffic came from
// inside that CIDR, so the rule excluded it — and a NetworkPolicy drops rather
// than rejects, so the page loaded forever and then reported
//
//	dial tcp 10.104.217.247:9090: i/o timeout
//
// after DNS had resolved perfectly well.
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

// The isolation rule that keeps tenants apart is untouched.
//
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
