// Command cloud is Atlantis Cloud's identity service.
//
// It holds the signing key, publishes the public half as a JWKS document, and
// mints the assertions consoles exchange for a session. Every console verifies
// against this issuer and has no other source of identity — there are no local
// accounts anywhere in the product.
//
// # Two modes, and why minting is not a route
//
//	cloud serve   publish the key set over HTTP
//	cloud mint    sign one assertion and print it
//
// Minting is a command rather than an endpoint on the server, and that is a
// deliberate constraint rather than an unfinished feature. An HTTP route that
// mints on request is a complete authentication bypass unless something in
// front of it establishes who is asking — and Cloud does not yet hold user
// records, so there is nothing to establish it with. Shipping the route first
// and the check afterwards would mean a window in which the strongest
// guarantee in the system is that anyone who can reach a port is an
// administrator of every organisation.
//
// `mint` requires read access to the signing key, so it is available to
// whoever operates Cloud and to nobody else. When Cloud grows a sign-in flow,
// that flow calls the same issuer package after authenticating the user.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rachitkumar205/atlantis/internal/cloud/identity"
	"github.com/rachitkumar205/atlantis/internal/cloud/issuer"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:], log)
	case "mint":
		err = mint(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "cloud: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "cloud: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `atlantis cloud — the identity service consoles verify against

usage:
  cloud serve [flags]   publish the JWKS document
  cloud mint  [flags]   sign one assertion and print it

run "cloud serve -h" or "cloud mint -h" for flags
`)
}

// keyFlag is shared by both commands: they must agree on which key, or the
// assertions minted by one will not verify against the set served by the other.
func keyFlag(fs *flag.FlagSet) *string {
	return fs.String("key", envOr("CLOUD_SIGNING_KEY", "./certs/cloud-signing-key.pem"),
		"path to the ECDSA signing key (created on first use)")
}

func issuerFlag(fs *flag.FlagSet) *string {
	return fs.String("issuer", os.Getenv("CLOUD_ISSUER"),
		"issuer name; becomes the iss claim and must match each console's CLOUD_ISSUER")
}

func serve(args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	keyPath := keyFlag(fs)
	issuerName := issuerFlag(fs)
	listen := fs.String("listen", envOr("CLOUD_LISTEN", ":9500"), "address to serve the JWKS document on")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *issuerName == "" {
		return errors.New("-issuer is required (or set CLOUD_ISSUER); it becomes the iss claim " +
			"and each console compares it for exact equality")
	}

	key, created, err := issuer.LoadOrCreateKey(*keyPath)
	if err != nil {
		return err
	}
	if created {
		log.Info("generated a signing key", "path", *keyPath, "kid", key.ID)
	}

	iss, err := issuer.New(*issuerName, key)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              *listen,
		Handler:           iss.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Graceful shutdown. A console that catches a truncated response while
	// refreshing its key set keeps its cached keys, so this is not
	// load-bearing for correctness — but a half-written JWKS is a confusing
	// thing to find in a log when something else is actually wrong.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() {
		log.Info("serving key set", "addr", *listen, "issuer", iss.Name(),
			"path", issuer.JWKSPath, "kid", key.ID)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

func mint(args []string) error {
	fs := flag.NewFlagSet("mint", flag.ExitOnError)
	keyPath := keyFlag(fs)
	issuerName := issuerFlag(fs)
	audience := fs.String("audience", os.Getenv("CLOUD_AUDIENCE"),
		"the console this assertion is for; must match its CLOUD_AUDIENCE")
	subject := fs.String("subject", "", "Cloud user id; becomes the audit actor")
	org := fs.String("org", "", "organisation the user is acting in")
	role := fs.String("role", string(identity.RoleAdmin), `"admin" or "viewer"`)
	email := fs.String("email", "", "email, shown in the console and on audit rows")
	name := fs.String("name", "", "display name (optional)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *issuerName == "" {
		return errors.New("-issuer is required (or set CLOUD_ISSUER)")
	}

	key, created, err := issuer.LoadOrCreateKey(*keyPath)
	if err != nil {
		return err
	}
	if created {
		// Worth saying plainly: a key made here is not the one `serve` is
		// publishing unless they were pointed at the same path, and an
		// assertion signed by an unpublished key is refused with a message
		// about the token rather than about the mismatch.
		fmt.Fprintf(os.Stderr, "cloud: generated a new signing key at %s — "+
			"if `cloud serve` is running against a different key, this assertion will not verify\n", *keyPath)
	}

	iss, err := issuer.New(*issuerName, key)
	if err != nil {
		return err
	}

	// Mint validates the grant and names the field that is missing, so there
	// is no argument checking to repeat here.
	token, err := iss.Mint(issuer.Grant{
		Subject:  *subject,
		Org:      *org,
		Role:     identity.Role(*role),
		Email:    *email,
		Name:     *name,
		Audience: *audience,
	})
	if err != nil {
		return err
	}

	fmt.Println(token)
	return nil
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
