package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// tideConfig is the shape of tide.yaml. Lives at the root of every caller repo.
//
// Example:
//
//	caller: api
//	org: acme
//	schema_paths:
//	  - internal/users
//	  - internal/orders
//
// The file describes the repository — its caller name, its organisation, which
// files are schema — and nothing about the deployment. The address and the
// credential come from `tide login`, which writes them under ~/.atlantis.
//
// ATL_CALLER and ATL_ORG override those two fields, for a checkout serving more
// than one.
type tideConfig struct {
	Caller      string   `yaml:"caller"`
	SchemaPaths []string `yaml:"schema_paths"`

	// TLS material, which nothing configures. The organisation owns the address
	// and issues the credential, and `tide login` collects both, so neither a
	// `tls:` key nor a TIDE_TLS_* variable is read.
	//
	// dial() fills these from the credential store. Nothing outside this file
	// writes them.
	//
	// A CI runner that cannot keep a key between runs therefore has no path:
	// pasting a private key into a repository secret is what enrolment replaced,
	// and nothing has taken its place.
	TLS struct {
		CertPEM string `yaml:"-"`
		KeyPEM  string `yaml:"-"`
		CAPEM   string `yaml:"-"`
	} `yaml:"-"`

	// Endpoint is filled from the credential store, never from configuration.
	Endpoint string `yaml:"-"`
	// Org names the organisation this repository's caller belongs to. See
	// resolveOrg: optional while exactly one organisation is enrolled.
	Org       string `yaml:"org"`
	OutputDir string `yaml:"output_dir"`
	// Generate lists the namespaces `tide generate` emits a typed client
	// for — the caller's own namespace plus any it consumes cross-namespace.
	Generate []string `yaml:"generate"`

	// storeDir and storeEnrollURL are set when the credentials came from the
	// credential store. They scope automatic renewal: tide renews only what tide
	// wrote, since it cannot write a replacement back to an environment
	// variable, and rotating an identity whose replacement it must discard locks
	// the caller out.
	//
	// Not YAML fields. A repository setting them would point renewal at a
	// directory it does not own.
	storeDir       string `yaml:"-"`
	storeEnrollURL string `yaml:"-"`
}

func loadPCConfig(path string) (*tideConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var c tideConfig
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	applyEnvOverrides(&c)
	if c.Caller == "" {
		return nil, fmt.Errorf("%s: `caller` is required", path)
	}
	// Fall back to the credential store for anything tide.yaml and the
	// environment did not supply.
	//
	// The only source. `tide login` is what puts material there.
	if err := applyStoreCredentials(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	// Replace the certificate if it is close to expiry. Best-effort and once
	// per process; see renewIfDue.
	renewIfDue(&c)
	if c.Endpoint == "" {
		return nil, fmt.Errorf("%s: this repository has no credentials.\n\n"+
			"Run `tide login` — the console's Callers page prints the command. "+
			"The address of your atlantis comes with them; it is not something "+
			"to configure here", path)
	}
	if len(c.SchemaPaths) == 0 {
		return nil, fmt.Errorf("%s: `schema_paths` must list at least one directory", path)
	}
	return &c, nil
}

func applyEnvOverrides(c *tideConfig) {
	if v := os.Getenv("ATL_CALLER"); v != "" {
		c.Caller = v
	}
	if v := os.Getenv("ATL_ORG"); v != "" {
		c.Org = v
	}
	if v := os.Getenv("ATL_GENERATE"); v != "" {
		c.Generate = nil
		for _, ns := range strings.Split(v, ",") {
			if ns = strings.TrimSpace(ns); ns != "" {
				c.Generate = append(c.Generate, ns)
			}
		}
	}
}

// Org is the organisation this repository's caller belongs to.
//
// Read from tide.yaml's `org:` or ATL_ORG. Optional for now: when it is absent
// and the credential store holds exactly one organisation, that one is used.
// Ambiguity is refused rather than guessed — applying schema to the wrong
// organisation is not a mistake worth being convenient about.
func (c *tideConfig) resolveOrg() (string, error) {
	if c.Org != "" {
		return c.Org, nil
	}
	orgs, err := orgsInStore()
	if err != nil {
		return "", err
	}
	switch len(orgs) {
	case 0:
		return "", errNoStoreOrg
	case 1:
		return orgs[0], nil
	default:
		return "", fmt.Errorf("this repository does not say which organisation it belongs to, "+
			"and %d are enrolled (%s).\n\nAdd `org:` to tide.yaml or set ATL_ORG",
			len(orgs), strings.Join(orgs, ", "))
	}
}

// errNoStoreOrg means there is nothing enrolled to fall back to. Not an error
// on its own — the caller may have credentials from tide.yaml or the
// environment, in which case the store is irrelevant.
var errNoStoreOrg = errors.New("no organisation enrolled")

// applyStoreCredentials fills in whatever the store has and the configuration
// did not.
//
// Per field rather than all-or-nothing, so a repository that pins one value —
// a CA for a proxy, say — still gets the rest. The certificate and key move
// together because they are one file and one credential.
func applyStoreCredentials(c *tideConfig) error {
	// Something already supplied a credential; the store has nothing to add and
	// should not be consulted, so a machine with no store is not an error.
	haveCert := c.TLS.CertPEM != ""
	haveEndpoint := c.Endpoint != ""
	if haveCert && haveEndpoint {
		return nil
	}

	org, err := c.resolveOrg()
	if errors.Is(err, errNoStoreOrg) {
		return nil
	}
	if err != nil {
		return err
	}

	creds, err := loadCredentials(org, c.Caller)
	if errors.Is(err, ErrNoCredentials) {
		// Nothing enrolled for this caller. Say what IS enrolled, because the
		// usual cause is a `caller:` that does not match what the token was
		// minted for, and the list makes that obvious.
		if others, lerr := callersInStore(org); lerr == nil && len(others) > 0 {
			return fmt.Errorf("no credentials for %s/%s. Enrolled in %s: %s.\n\n"+
				"Run `tide login` for this caller, or correct `caller:`",
				org, c.Caller, org, strings.Join(others, ", "))
		}
		return nil
	}
	if err != nil {
		return err
	}

	if !haveCert {
		// One file, handed to both halves. tls.X509KeyPair scans for a
		// CERTIFICATE block and for a "PRIVATE KEY" block independently, so the
		// same bytes satisfy each.
		c.TLS.CertPEM = string(creds.ClientPEM)
		c.TLS.KeyPEM = string(creds.ClientPEM)
		// Only now: renewal is scoped to material tide owns, and it owns this
		// only because nothing else supplied a certificate.
		c.storeDir = creds.Dir
		c.storeEnrollURL = creds.EnrollURL
	}
	if c.TLS.CAPEM == "" {
		c.TLS.CAPEM = string(creds.CAPEM)
	}
	if !haveEndpoint {
		c.Endpoint = creds.Endpoint
	}
	return nil
}
