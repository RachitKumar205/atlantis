// Command cloud is Atlantis Cloud's control plane.
//
// It holds the signing key, publishes the public half as a JWKS document, and
// mints the assertions consoles exchange for a session. Every console verifies
// against this issuer and has no other source of identity — there are no local
// accounts anywhere in the product. It also registers organisations, which is
// the other half of the same job: Cloud says who a user is, and Cloud says
// which atlantis that user's organisation is served by.
//
//	cloud serve          publish the key set over HTTP
//	cloud mint           sign one assertion and print it
//	cloud org register   record an organisation's atlantis and credentials
//	cloud data-key       print a keyset for a console's CONSOLE_DATA_KEY
//
// # Why minting is not a route
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
//
// # Why registration is not a route either
//
// `org register` is the same argument one layer along. Registering an
// organisation decides which atlantis a console will hand that organisation's
// users, so an unauthenticated route for it would let anyone who can reach the
// console repoint an organisation at a server they control — and every request
// afterwards would succeed, because the credentials would be genuine.
//
// It is here rather than in the console because Cloud is the thing that
// provisions organisations, and because this binary already runs where an
// operator runs it. The cost is that cmd/cloud now opens a database, which is
// the first time anything in Cloud's neighbourhood does.
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
	"github.com/rachitkumar205/atlantis/internal/cloud/store"
	"github.com/rachitkumar205/atlantis/internal/console"
	"github.com/rachitkumar205/atlantis/internal/console/secrets"
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
	case "org":
		err = org(os.Args[2:], log)
	case "user":
		err = user(os.Args[2:], log)
	case "member":
		err = member(os.Args[2:], log)
	case "data-key":
		err = dataKey(os.Args[2:])
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
	fmt.Fprint(os.Stderr, `atlantis cloud — the control plane consoles verify against

usage:
  cloud serve [flags]          publish the JWKS document
  cloud mint  [flags]          sign one assertion and print it

  cloud user create [flags]    create an account
  cloud org create [flags]     record that an organisation exists
  cloud member add [flags]     grant an account a role in an organisation
  cloud member remove [flags]  revoke it

  cloud org register [flags]   point an organisation at its atlantis
  cloud data-key               print a keyset for a console's CONSOLE_DATA_KEY

run any command with -h for its flags
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

// ── Cloud's own database ────────────────────────────────────────────────────

// openCloud connects to Cloud's database, applying any pending migrations
// first.
//
// Migrating on connect rather than in a separate command: Cloud owns this
// schema outright, it is the only writer, and a binary that is newer than its
// database is the state every one of these commands would otherwise fail in
// with a message about a missing column. The console does the same at startup.
func openCloud(ctx context.Context, dbURL string, log *slog.Logger) (*store.Store, error) {
	if dbURL == "" {
		return nil, errors.New("-db is required (or set CLOUD_PG_URL): this reads and writes Cloud's own database")
	}
	if err := store.Migrate(dbURL, log); err != nil {
		return nil, fmt.Errorf("migrate cloud schema: %w", err)
	}
	db, err := store.New(ctx, dbURL, log)
	if err != nil {
		return nil, fmt.Errorf("open cloud db: %w", err)
	}
	// Before anything writes: refuse a schema where a table carries per-user
	// rows with no boundary and no recorded decision. See store.VerifyPolicies.
	if err := store.VerifyPolicies(ctx, db.Pool()); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func cloudDBFlag(fs *flag.FlagSet) *string {
	return fs.String("db", os.Getenv("CLOUD_PG_URL"), "Cloud's database URL (or set CLOUD_PG_URL)")
}

// ── Accounts ────────────────────────────────────────────────────────────────

func user(args []string, log *slog.Logger) error {
	if len(args) == 0 {
		return errors.New(`cloud user: expected a subcommand (create)`)
	}
	switch args[0] {
	case "create":
		return userCreate(args[1:], log)
	default:
		return fmt.Errorf("cloud user: unknown subcommand %q", args[0])
	}
}

func userCreate(args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("user create", flag.ExitOnError)
	email := fs.String("email", "", "email address; folded to lowercase and unique")
	name := fs.String("name", "", "display name (optional)")
	dbURL := cloudDBFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *email == "" {
		return errors.New("-email is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := openCloud(ctx, *dbURL, log)
	if err != nil {
		return err
	}
	defer db.Close()

	u, err := db.CreateUser(ctx, *email, *name, nil)
	if errors.Is(err, store.ErrAlreadyExists) {
		// Reported and successful, so re-running a seeding script is not an
		// error — matching `org create`, which upserts for the same reason. The
		// alternative a script reaches for is swallowing every failure from
		// this command, which also swallows the ones that matter.
		existing, lookupErr := db.UserByEmail(ctx, *email)
		if lookupErr != nil {
			return err
		}
		fmt.Println(existing.ID)
		fmt.Fprintf(os.Stderr, "cloud: %s already exists, unchanged\n", existing.Email)
		return nil
	}
	if err != nil {
		return err
	}

	fmt.Println(u.ID)
	// Said plainly rather than left to be discovered: this account exists and
	// cannot sign in. An operator who creates one and then cannot get in should
	// find out here, not at a sign-in page that refuses without saying why.
	fmt.Fprintf(os.Stderr, "cloud: created %s with no credential — it cannot sign in "+
		"until a password is set or an account is linked\n", u.Email)
	return nil
}

// ── Membership ──────────────────────────────────────────────────────────────

func member(args []string, log *slog.Logger) error {
	if len(args) == 0 {
		return errors.New(`cloud member: expected a subcommand (add, remove)`)
	}
	switch args[0] {
	case "add":
		return memberChange(args[1:], true, log)
	case "remove":
		return memberChange(args[1:], false, log)
	default:
		return fmt.Errorf("cloud member: unknown subcommand %q", args[0])
	}
}

func memberChange(args []string, add bool, log *slog.Logger) error {
	verb := "remove"
	if add {
		verb = "add"
	}
	fs := flag.NewFlagSet("member "+verb, flag.ExitOnError)
	email := fs.String("email", "", "the account's email address")
	orgName := fs.String("org", "", "organisation")
	role := fs.String("role", string(identity.RoleAdmin), `"admin" or "viewer"`)
	dbURL := cloudDBFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *email == "" || *orgName == "" {
		return errors.New("-email and -org are both required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := openCloud(ctx, *dbURL, log)
	if err != nil {
		return err
	}
	defer db.Close()

	u, err := db.UserByEmail(ctx, *email)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no account for %s — create it with `cloud user create`", *email)
	}
	if err != nil {
		return err
	}

	if !add {
		if err := db.RemoveMember(ctx, u.ID, *orgName); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("%s is not a member of %s", *email, *orgName)
			}
			return err
		}
		fmt.Printf("removed %s from %s\n", *email, *orgName)
		return nil
	}

	if err := db.AddMember(ctx, u.ID, *orgName, identity.Role(*role)); err != nil {
		return err
	}
	fmt.Printf("%s is %s of %s\n", *email, *role, *orgName)
	return nil
}

// org dispatches the organisation subcommands.
//
// Spelled `org create` / `org register` rather than flat verbs because the two
// are genuinely different things and conflating them caused confusion once
// already: `create` records that an organisation exists and who may act in it;
// `register` says which atlantis serves it. An organisation can exist for a
// while before it is provisioned.
func org(args []string, log *slog.Logger) error {
	if len(args) == 0 {
		return errors.New(`cloud org: expected a subcommand (create, register)`)
	}
	switch args[0] {
	case "create":
		return orgCreate(args[1:], log)
	case "register":
		return orgRegister(args[1:], log)
	default:
		return fmt.Errorf("cloud org: unknown subcommand %q", args[0])
	}
}

func orgCreate(args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("org create", flag.ExitOnError)
	name := fs.String("org", "", "organisation name: lowercase, alphanumeric and hyphens, max 63")
	display := fs.String("display-name", "", "human-readable name (optional)")
	dbURL := cloudDBFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return errors.New("-org is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := openCloud(ctx, *dbURL, log)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := db.CreateOrg(ctx, *name, *display); err != nil {
		return err
	}
	fmt.Printf("organisation %s exists\n", *name)
	fmt.Fprintln(os.Stderr, "cloud: it has no atlantis yet — `cloud org register` points it at one")
	return nil
}

func orgRegister(args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("org register", flag.ExitOnError)
	name := fs.String("org", "", "organisation name; must match the org claim Cloud mints for its users")
	endpoint := fs.String("endpoint", "", "host:port of this organisation's atlantis admin gRPC service")
	health := fs.String("health", "", "host:port of the same server's plain-HTTP health endpoint")
	caPath := fs.String("ca", "", "PEM bundle the console verifies this organisation's atlantis against")
	certPath := fs.String("cert", "", "PEM client certificate the console presents to it")
	keyPath := fs.String("key", "", "PEM private key for -cert")
	dbURL := fs.String("db", os.Getenv("CONSOLE_PG_URL"), "console database URL (or set CONSOLE_PG_URL)")
	cloudDBURL := fs.String("cloud-db", os.Getenv("CLOUD_PG_URL"),
		"Cloud's database URL (or set CLOUD_PG_URL)")
	keyset := fs.String("data-key", os.Getenv("CONSOLE_DATA_KEY"),
		"the console's keyset, base64 (or set CONSOLE_DATA_KEY)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	// Named individually rather than as "missing arguments", because six flags
	// is enough that "which one" is the actual question.
	for _, r := range []struct{ flag, val string }{
		{"-org", *name},
		{"-endpoint", *endpoint},
		{"-health", *health},
		{"-ca", *caPath},
		{"-cert", *certPath},
		{"-key", *keyPath},
	} {
		if r.val == "" {
			return fmt.Errorf("%s is required", r.flag)
		}
	}
	if *dbURL == "" {
		return errors.New("-db is required (or set CONSOLE_PG_URL): this writes to the console's database")
	}
	if *keyset == "" {
		return errors.New("-data-key is required (or set CONSOLE_DATA_KEY): the private key is " +
			"sealed with it, and it must be the same keyset the console serves with, or the row " +
			"will be written and never open")
	}

	ca, err := os.ReadFile(*caPath)
	if err != nil {
		return fmt.Errorf("read -ca: %w", err)
	}
	cert, err := os.ReadFile(*certPath)
	if err != nil {
		return fmt.Errorf("read -cert: %w", err)
	}
	key, err := os.ReadFile(*keyPath)
	if err != nil {
		return fmt.Errorf("read -key: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Two databases, and the order is deliberate.
	//
	// Cloud's row first, because it is the cheap one to repeat and the one an
	// operator can inspect. If the console write then fails, the organisation
	// exists in Cloud with nobody able to reach an atlantis for it — which is
	// the *ordinary* state between `org create` and provisioning, so it is a
	// state the product already handles and a message already exists for.
	//
	// The other order produces a state nothing handles: a console that can dial
	// an organisation Cloud has never heard of, so no assertion naming it will
	// ever be minted and the row is unreachable and invisible.
	//
	// Neither write is atomic with the other and they are in different
	// databases, so nothing here can make them so. Both are upserts, so the fix
	// for a partial run is to run it again.
	cloudDB, err := openCloud(ctx, *cloudDBURL, log)
	if err != nil {
		return err
	}
	defer cloudDB.Close()

	if err := cloudDB.CreateOrg(ctx, *name, ""); err != nil {
		return fmt.Errorf("record %s in Cloud: %w", *name, err)
	}

	// RegisterOrg validates the material before it writes, so a swapped
	// -cert/-key or an expired leaf is refused here rather than found later as
	// a 503 by whoever next opens the console.
	if err := console.RegisterOrg(ctx, *dbURL, *keyset, console.OrgRegistration{
		Org:        *name,
		Endpoint:   *endpoint,
		HealthAddr: *health,
		CAPEM:      string(ca),
		CertPEM:    string(cert),
		KeyPEM:     key,
	}); err != nil {
		return fmt.Errorf("%w\n\n%s exists in Cloud but has no atlantis registered. "+
			"Both writes are upserts — re-run this command once the problem above "+
			"is fixed", err, *name)
	}

	fmt.Printf("registered %s at %s\n", *name, *endpoint)
	// Said every time, because it is the one thing this command cannot check.
	// A running console re-reads the row on its own schedule, so an operator
	// who rotates a certificate and immediately tests it may see the old one
	// and conclude the registration failed.
	fmt.Fprintln(os.Stderr, "cloud: a running console picks this up within five minutes")
	return nil
}

// dataKey prints a keyset for CONSOLE_DATA_KEY.
//
// One command, no flags, and it writes to stdout so it can be captured. There
// is deliberately no `-write` that puts it in a file: the value belongs in
// whatever holds the deployment's secrets, and a command that drops key
// material on disk invites it being left there.
//
// Losing this value is not recoverable. Every organisation's private key is
// sealed under it, so a console started with a different one has rows that are
// intact, complete, and permanently unopenable — which presents as every
// organisation being unreachable, with nothing in the schema looking wrong.
func dataKey(args []string) error {
	fs := flag.NewFlagSet("data-key", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	k, err := secrets.NewKeyset()
	if err != nil {
		return err
	}
	fmt.Println(k)
	fmt.Fprintln(os.Stderr, "cloud: store this where the deployment's other secrets live. "+
		"Every organisation's private key is sealed under it, and there is no way to recover them without it.")
	return nil
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
