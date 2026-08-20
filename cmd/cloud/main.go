// Command cloud is Atlantis Cloud's control plane.
//
// It holds the signing key, publishes the public half as a JWKS document, and
// mints the assertions consoles exchange for a session. Every console verifies
// against this issuer and has no other source of identity — there are no local
// accounts anywhere in the product. It also registers organisations, which is
// the other half of the same job: Cloud says who a user is, and Cloud says
// which atlantis that user's organisation is served by.
//
//	cloud serve          publish the key set, and serve the account routes
//	cloud mint           sign one assertion and print it
//	cloud org register   record an organisation's atlantis and credentials
//	cloud data-key       print a keyset for a console's CONSOLE_DATA_KEY
//
// # What `serve` does not serve
//
// Sign-in. Cloud's sign-in is two-legged — a password, then a second factor —
// and the second factor does not exist yet. A route that issued a session on a
// password alone would be the posture this product refuses, so it lands with
// the factor that gates it. Nothing `serve` exposes today creates a session.
//
// # Minting is now a route as well, and why `mint` is still here
//
// This doc used to argue that minting could not be an endpoint, because "an
// HTTP route that mints on request is a complete authentication bypass unless
// something in front of it establishes who is asking — and Cloud does not yet
// hold user records, so there is nothing to establish it with."
//
// That premise expired. Cloud holds accounts, sessions and a second factor, so
// `GET /authorize` establishes who is asking from a session and reads the role
// out of cloud.memberships rather than taking it as a parameter. The condition
// the old comment set was met before the route was built, which is the order it
// asked for.
//
// `mint` stays because a route needs a browser and a session, and two cases
// have neither: an operator diagnosing a deployment, and the first membership
// in a new one — the person who has to be let in before anybody can let anybody
// in. It is no longer an escape hatch around the gate, though. It opens the
// database, reads the same membership row /authorize reads, and refuses when
// there is none. There is no `-role` flag: the row decides, so that the command
// and the route cannot disagree about what somebody is allowed to be.
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
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rachitkumar205/atlantis/internal/cloud/identity"
	"github.com/rachitkumar205/atlantis/internal/cloud/issuer"
	cloudsrv "github.com/rachitkumar205/atlantis/internal/cloud/server"
	"github.com/rachitkumar205/atlantis/internal/cloud/store"
	"github.com/rachitkumar205/atlantis/internal/console"
	"github.com/rachitkumar205/atlantis/internal/secrets"
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

// serve runs Cloud's HTTP surface: the key set every console verifies against,
// and the account routes.
//
// Configuration comes from the environment rather than flags here, because
// there is now more of it than a command line wants to carry and because it is
// the same set a deployment sets once. The flags that remain are the two an
// operator overrides interactively.
func serve(args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	keyPath := keyFlag(fs)
	issuerName := issuerFlag(fs)
	listen := fs.String("listen", "", "address to serve on (overrides CLOUD_LISTEN)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := cloudsrv.ConfigFromEnv()
	if err != nil {
		return err
	}
	// Flags win where they were given, so `cloud serve -issuer X` still works
	// the way it did before this grew a config.
	if *issuerName != "" {
		cfg.Issuer = *issuerName
	}
	if *keyPath != "" {
		cfg.SigningKey = *keyPath
	}
	if *listen != "" {
		cfg.Listen = *listen
	}

	key, created, err := issuer.LoadOrCreateKey(cfg.SigningKey)
	if err != nil {
		return err
	}
	if created {
		log.Info("generated a signing key", "path", cfg.SigningKey, "kid", key.ID)
	}
	iss, err := issuer.New(cfg.Issuer, key)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := openCloud(ctx, cfg.PGURL, log)
	if err != nil {
		return err
	}
	defer db.Close()

	// spa_embed.go or spa_none.go, depending on the embedspa build tag.
	sub, err := spaFS()
	if err != nil {
		return fmt.Errorf("embed the sign-in app: %w", err)
	}

	api, err := cloudsrv.New(cfg, db, iss, sub, log)
	if err != nil {
		return err
	}
	defer api.Close()

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           api,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		// spa_embedded says whether this binary carries the sign-in
		// application. False is normal for a development build and wrong for a
		// deployed one, and it is the only place the difference is visible
		// before somebody loads a page and gets a 404.
		log.Info("serving", "addr", cfg.Listen, "issuer", iss.Name(),
			"jwks", issuer.JWKSPath, "kid", key.ID, "public_url", cfg.PublicURL,
			"spa_embedded", sub != nil)
		if cfg.SMTPAddr == "" {
			// Said at startup as well as at every send, because this is the
			// setting whose absence looks like everything working: accounts are
			// created, the response says a message is on its way, and the link
			// is in a log nobody reads.
			log.Warn("no CLOUD_SMTP_ADDR — verification and reset links will be " +
				"written to this log instead of emailed")
		}
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	// Graceful shutdown. A console that catches a truncated response while
	// refreshing its key set keeps its cached keys, so this is not
	// load-bearing for correctness — but a half-written JWKS is a confusing
	// thing to find in a log when something else is actually wrong.
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

// mint prints an assertion for a member of an organisation.
//
// # What changed when /authorize arrived
//
// It used to take -subject, -org, -role, -email and -audience and sign them.
// That was a token saying whatever the operator typed, which was tolerable only
// because typing it required the signing key.
//
// Now it takes an account and an organisation and reads the rest. The role
// comes from cloud.memberships and the audience from cloud.orgs.console_url —
// the same two rows /authorize consults — so an assertion from the command line
// and one from a browser carry the same authority for the same person. A
// -role flag would be a way for those two to disagree, so there is not one.
//
// It refuses when there is no membership. That is the point: the gate is the
// row, and a command that could skip it would mean the gate is optional.
func mint(args []string) error {
	fs := flag.NewFlagSet("mint", flag.ExitOnError)
	keyPath := keyFlag(fs)
	issuerName := issuerFlag(fs)
	dbURL := cloudDBFlag(fs)
	audience := fs.String("audience", "",
		"override the console this assertion is for; defaults to the organisation's registered console")
	email := fs.String("email", "", "the account to mint for")
	org := fs.String("org", "", "organisation the user is acting in")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *issuerName == "" {
		return errors.New("-issuer is required (or set CLOUD_ISSUER)")
	}
	if *email == "" || *org == "" {
		return errors.New("-email and -org are required")
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	ctx := context.Background()
	db, err := openCloud(ctx, *dbURL, log)
	if err != nil {
		return err
	}
	defer db.Close()

	user, err := db.UserByEmail(ctx, *email)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no account for %s", *email)
	}
	if err != nil {
		return err
	}

	// The gate, and the same one /authorize applies.
	role, err := db.RoleIn(ctx, user.ID, *org)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%s is not a member of %s: add them with `cloud member add`", *email, *org)
	}
	if err != nil {
		return err
	}

	// The registered console is both the destination and the audience, so
	// defaulting from it keeps a hand-minted token verifiable at the same place
	// a browser would have been sent. -audience stays for the case where an
	// operator is testing a console that is not the registered one.
	if *audience == "" {
		*audience, err = db.ConsoleURL(ctx, *org)
		if errors.Is(err, store.ErrNoConsole) {
			return fmt.Errorf("%s has no registered console: "+
				"run `cloud org register -console-url …`, or pass -audience", *org)
		}
		if err != nil {
			return err
		}
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
	//
	// StepUp is deliberately absent: a command cannot present a second factor,
	// so a token from here does not claim one was. It signs somebody in; it
	// does not elevate them.
	token, err := iss.Mint(issuer.Grant{
		Subject:  user.ID,
		Org:      *org,
		Role:     role,
		Email:    user.Email,
		Name:     user.Name,
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
	consoleURL := fs.String("console-url", "",
		"absolute URL of this organisation's console; also the assertion audience")
	endpoint := fs.String("endpoint", "", "host:port of this organisation's atlantis admin gRPC service")
	health := fs.String("health", "", "host:port of the same server's plain-HTTP health endpoint")
	// Optional, and the only flag here that is. -endpoint is the address the
	// CONSOLE dials; this is the one handed to callers at enrolment, for the day
	// those differ — the console may sit inside a network a developer's laptop
	// does not. Unset means they are the same, which is true of every deployment
	// today and is what the column's COALESCE expresses.
	publicEndpoint := fs.String("caller-endpoint", "",
		"host:port callers dial, if different from -endpoint (default: same)")
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
		{"-console-url", *consoleURL},
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

	// Parsed here as well as CHECKed in the database, because the message an
	// operator can act on is this one. A trailing slash is trimmed so that the
	// value stored is the one compared against the console's CLOUD_AUDIENCE,
	// where a stray slash would produce a token every console rejects with no
	// hint as to why.
	*consoleURL = strings.TrimRight(*consoleURL, "/")
	if u, err := url.Parse(*consoleURL); err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("-console-url must be an absolute URL like "+
			"https://acme.console.example, got %q", *consoleURL)
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
	if err := cloudDB.SetConsoleURL(ctx, *name, *consoleURL); err != nil {
		return fmt.Errorf("record %s's console: %w", *name, err)
	}

	// RegisterOrg validates the material before it writes, so a swapped
	// -cert/-key or an expired leaf is refused here rather than found later as
	// a 503 by whoever next opens the console.
	if err := console.RegisterOrg(ctx, *dbURL, *keyset, console.OrgRegistration{
		Org:            *name,
		Endpoint:       *endpoint,
		HealthAddr:     *health,
		CAPEM:          string(ca),
		CertPEM:        string(cert),
		KeyPEM:         key,
		PublicEndpoint: *publicEndpoint,
	}); err != nil {
		return fmt.Errorf("%w\n\n%s exists in Cloud but has no atlantis registered. "+
			"Both writes are upserts — re-run this command once the problem above "+
			"is fixed", err, *name)
	}

	fmt.Printf("registered %s at %s\n", *name, *endpoint)
	// Printed so the value an operator has to put in the console's
	// CLOUD_AUDIENCE comes from the command that set it, rather than from
	// somebody retyping it. The two must be identical: the audience Cloud mints
	// is this string, and a console checks `aud` against its own configured
	// value, so a difference of one character is every sign-in failing with a
	// message about the token.
	fmt.Printf("set CLOUD_AUDIENCE=%s on that console\n", *consoleURL)
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
