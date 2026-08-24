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
//	cloud org create     create an organisation and queue it for provisioning
//	cloud org status     show how far provisioning has got
//	cloud org register   record an organisation's atlantis and credentials
//	cloud org revoke-console   cut the console off from one organisation
//	cloud org restore-console  give it back, grants intact
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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/internal/cloud/identity"
	"github.com/rachitkumar205/atlantis/internal/cloud/issuer"
	"github.com/rachitkumar205/atlantis/internal/cloud/provision/certs"
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
	case "signing-key":
		err = signingKey(os.Args[2:])
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
  cloud org create [flags]     create an organisation and queue it for provisioning
  cloud org status [flags]     show how far provisioning has got
  cloud member add [flags]     grant an account a role in an organisation
  cloud member remove [flags]  revoke it

  cloud org register [flags]   point an organisation at an atlantis built by hand
  cloud signing-key -path P    create the assertion signing key, if absent
  cloud data-key               print a keyset for a console's CONSOLE_DATA_KEY

  cloud org revoke-console     cut the console off from one organisation
  cloud org restore-console    give it back; grants are kept, so nothing is rebuilt

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
		// Points at signing up rather than at `cloud user create`, which makes
		// an account with no password that cannot then be signed up for — the
		// sign-up form takes its already-exists branch and sends no
		// verification link, so the browser shows success and nothing arrives.
		// `make dev-cloud-seed` stopped calling it for that reason, and this
		// message is the other half of the same correction.
		return fmt.Errorf("no account for %s — sign up first, then run this to grant membership", *email)
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
		return errors.New(`cloud org: expected a subcommand (create, status, register, purge, revoke-console, restore-console)`)
	}
	switch args[0] {
	case "create":
		return orgCreate(args[1:], log)
	case "status":
		return orgStatus(args[1:], log)
	case "register":
		return orgRegister(args[1:], log)
	case "purge":
		return orgPurge(args[1:], log)
	case "revoke-console":
		return orgRevokeConsole(args[1:], log)
	case "restore-console":
		return orgRestoreConsole(args[1:], log)
	default:
		return fmt.Errorf("cloud org: unknown subcommand %q", args[0])
	}
}

// orgCreate records an organisation, grants its owner, and queues it for
// provisioning.
//
// # Why -owner is required rather than a second command
//
// It used to write the organisation row and nothing else, leaving `cloud member
// add` to grant somebody access. That ordering produced a state the product
// answers badly: /authorize checks membership BEFORE it checks whether an
// atlantis is registered, so an organisation with no member is a 403 —
// "you are not a member" — no matter how perfectly it was provisioned.
//
// Making the owner optional would keep that state one flag away, and the whole
// point of this step is that it should not be reachable. `make dev-cloud-seed`
// ran exactly these two commands in sequence and is now one call.
//
// The owner must already have an account, which is the same constraint `cloud
// member add` has always had. An organisation owned by an address nobody has
// verified is an organisation nobody can enter.
func orgCreate(args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("org create", flag.ExitOnError)
	name := fs.String("org", "", "organisation name: lowercase, alphanumeric and hyphens, max 63")
	display := fs.String("display-name", "", "human-readable name (optional)")
	owner := fs.String("owner", "", "email address of the first admin; the account must already exist")
	wait := fs.Bool("wait", false, "block until the organisation is serving, or until provisioning fails")
	waitFor := fs.Duration("wait-timeout", 10*time.Minute, "how long -wait waits before giving up")
	dbURL := cloudDBFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	for _, r := range []struct{ flag, val string }{
		{"-org", *name},
		{"-owner", *owner},
	} {
		if r.val == "" {
			return fmt.Errorf("%s is required", r.flag)
		}
	}

	// The context has to outlive the wait, not the create. Sized from the flag
	// with room for the writes either side, so -wait-timeout is what decides
	// when this gives up rather than a constant somebody has to find.
	timeout := 30 * time.Second
	if *wait {
		timeout = *waitFor + 30*time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	db, err := openCloud(ctx, *dbURL, log)
	if err != nil {
		return err
	}
	defer db.Close()

	u, err := db.UserByEmail(ctx, *owner)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no account for %s — sign up first, then run this to create "+
			"the organisation they will own", *owner)
	}
	if err != nil {
		return err
	}

	if err := db.CreateOrgWithOwner(ctx, *name, *display, u.ID, identity.RoleAdmin); err != nil {
		return err
	}

	fmt.Printf("organisation %s exists, owned by %s\n", *name, *owner)

	if !*wait {
		fmt.Fprintf(os.Stderr, "cloud: queued for provisioning — `cloud org status -org %s` "+
			"follows it, and `cloud org register` is only needed for an atlantis the "+
			"provisioner did not build\n", *name)
		return nil
	}
	return waitForProvisioning(ctx, db, *name, *waitFor)
}

// waitForProvisioning blocks until the organisation is serving, or reports why
// it is not.
//
// # Why this exits non-zero
//
// Because a wait that reports success for an organisation that never came up is
// the exact failure this flag exists to detect, reproduced inside the tool built
// to detect it. A Makefile target or a walkthrough step would carry straight on
// and fail later, somewhere less informative.
//
// # Why a `failed` state stops the wait
//
// `failed` is not terminal — the provisioner retries it after a backoff — so
// this could keep waiting. It does not, because the failures that reach this
// state locally are the ones a retry does not fix: a bad image reference, a
// cluster that is not running, a setting nobody has filled in. Reporting the
// reason now beats reporting a timeout in ten minutes, and the message says the
// retry is still coming.
func waitForProvisioning(ctx context.Context, db *store.Store, org string, limit time.Duration) error {
	const poll = 2 * time.Second
	deadline := time.Now().Add(limit)

	var last store.ProvisioningState
	for {
		p, err := db.ProvisioningFor(ctx, org)
		if err != nil {
			return fmt.Errorf("read the provisioning state of %s: %w", org, err)
		}
		if p.State != last {
			// Only on change, so a ten-minute wait is a handful of lines rather
			// than three hundred identical ones.
			fmt.Fprintf(os.Stderr, "cloud: %s is %s\n", org, p.State)
			last = p.State
		}

		switch p.State {
		case store.StateReady:
			fmt.Printf("%s is serving", org)
			if u, err := db.ConsoleURL(ctx, org); err == nil && u != "" {
				fmt.Printf(" — sign in at %s", u)
			}
			fmt.Println()
			return nil
		case store.StateFailed:
			return fmt.Errorf("provisioning %s failed after %d attempt(s): %s\n"+
				"it will be retried automatically; `cloud org status -org %s` shows the current state",
				org, p.Attempts, p.LastError, org)
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("%s was not provisioned within %s (state %q, %d attempt(s)) — "+
				"it is still queued, so this is a timeout on waiting rather than a failure; "+
				"`cloud org status -org %s` follows it",
				org, limit, p.State, p.Attempts, org)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// orgStatus prints where an organisation has got to.
//
// The queue row is otherwise readable only with psql, which makes a failed
// organisation diagnosable by whoever has database access and nobody else.
// attempts and last_error are the two fields that distinguish "still coming up"
// from "has been failing since Tuesday" — the same pair the organisations
// screen will read.
func orgStatus(args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("org status", flag.ExitOnError)
	name := fs.String("org", "", "organisation name")
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

	p, err := db.ProvisioningFor(ctx, *name)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%s is not queued for provisioning — either it does not exist, "+
			"or it was registered by hand with `cloud org register`", *name)
	}
	if err != nil {
		return err
	}

	fmt.Printf("org         %s\n", p.Org)
	fmt.Printf("state       %s\n", p.State)
	fmt.Printf("attempts    %d\n", p.Attempts)
	if p.ClaimedBy != "" {
		fmt.Printf("claimed by  %s\n", p.ClaimedBy)
	}
	if p.ClaimedUntil != nil {
		fmt.Printf("lease until %s\n", p.ClaimedUntil.Format(time.RFC3339))
	}
	if p.NextAttemptAfter != nil {
		fmt.Printf("next try    %s\n", p.NextAttemptAfter.Format(time.RFC3339))
	}
	if u, err := db.ConsoleURL(ctx, *name); err == nil && u != "" {
		fmt.Printf("console     %s\n", u)
	}
	if p.LastError != "" {
		// Kept after a later success rather than cleared, so this is printed
		// whatever the state — "this took four goes and here is what was wrong"
		// is worth more than a tidy row.
		fmt.Printf("last error  %s\n", p.LastError)
	}
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

// signingKey creates the key Cloud mints assertions with, if it does not exist.
//
// # Why this exists as a command
//
// The key was created lazily by `cloud serve`, on the theory that a development
// stack has one and it appears the first time anybody runs it. That held while
// Cloud only ever ran on the machine that owned the file.
//
// It stopped holding when Cloud moved into the cluster. The Deployment gets the
// key from a Secret, and the Secret is filled from this file at deploy time —
// so on a machine that has never run `cloud serve`, the file is absent, the
// Secret cannot be written, and `make dev-k8s-load` skips Cloud entirely. From
// a clean checkout that is a dead end: the thing that would create the key is
// the thing that will not start without it.
//
// So the creation is its own step, ahead of the deploy, exactly as the data key
// already is.
//
// Idempotent, because LoadOrCreateKey is. Running it twice keeps the first key
// — which matters more than it sounds: replacing it invalidates every assertion
// in flight and every session, and the failure presents as everybody being
// signed out at once with nothing in the logs about a key.
func signingKey(args []string) error {
	fs := flag.NewFlagSet("signing-key", flag.ExitOnError)
	path := fs.String("path", "", "where to write the key (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" {
		return errors.New("-path is required: this writes a private key, and " +
			"choosing where is not something to default")
	}

	k, created, err := issuer.LoadOrCreateKey(*path)
	if err != nil {
		return err
	}
	if created {
		fmt.Printf("wrote a new signing key to %s (kid %s)\n", *path, k.ID)
		fmt.Fprintln(os.Stderr, "cloud: back this up with the deployment's other secrets. "+
			"Anyone holding it can mint an assertion for any account in any "+
			"organisation, and losing it signs everybody out at once.")
		return nil
	}
	fmt.Printf("%s already exists (kid %s); keeping it\n", *path, k.ID)
	return nil
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// orgPurge brings an organisation's destruction forward to immediately.
//
// # Why this exists
//
// Deleting from the browser is deliberately reversible: it stops the
// organisation serving, starts a thirty-day clock, and leaves the namespace
// alone. That window is the only safety net the system has while there are no
// database backups.
//
// It also means the row survives for thirty days, holding the name and counting
// against its creator's limit. So "delete it and make a fresh one with the same
// name" is otherwise a month's wait. This is the operator's answer to that, and
// to a customer who asks to be erased now rather than eventually.
//
// # What it does not do
//
// It does not destroy anything itself. It sets purge_after to now; the
// provisioner tears the namespace down on its next reconcile pass, because the
// provisioner is the only process in this system that holds Kubernetes
// credentials — see internal/provisioner's package comment for why that
// separation is not negotiable.
//
// So this needs a database URL and no kubeconfig, and the destruction is
// visible in the provisioner's log rather than this one.
//
// # The guard
//
// -yes, and the name as -org. Two deliberate acts, because there is no
// confirmation prompt: this is expected to run in a terminal where a
// half-remembered shell history entry is one arrow key away.
func orgPurge(args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("org purge", flag.ExitOnError)
	name := fs.String("org", "", "organisation name")
	yes := fs.Bool("yes", false, "required: confirm that this destroys the organisation permanently")
	dbURL := cloudDBFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return errors.New("-org is required")
	}
	if !*yes {
		return fmt.Errorf("refusing to purge %s without -yes: this destroys its "+
			"database, its certificate authority and every schema in it, and there "+
			"are no backups to restore from", *name)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	db, err := openCloud(ctx, *dbURL, log)
	if err != nil {
		return err
	}
	defer db.Close()

	// Read first, so the operator is told what they are about to destroy rather
	// than only that something happened. A name typed from memory that matches
	// nothing should say so before anything is written.
	p, err := db.ProvisioningFor(ctx, *name)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%s is not queued for provisioning — either it does not "+
			"exist, or it was registered by hand with `cloud org register` and has "+
			"no row for the provisioner to act on", *name)
	}
	if err != nil {
		return err
	}
	fmt.Printf("purging %s (currently %s)\n", p.Org, p.State)

	if err := db.PurgeNow(ctx, *name); err != nil {
		return err
	}

	fmt.Printf("%s is queued for destruction.\n", *name)
	fmt.Println()
	fmt.Println("    The provisioner destroys it on its next reconcile pass, which is")
	fmt.Println("    PROVISIONER_RECONCILE_INTERVAL away — five minutes by default. Watch")
	fmt.Println("    the provisioner's log, not this one.")
	fmt.Println()
	fmt.Println("    Until then it can still be brought back with:")
	fmt.Printf("      UPDATE cloud.org_provisioning SET state = 'ready',\n")
	fmt.Printf("             deleted_at = NULL, purge_after = NULL WHERE org = '%s';\n", *name)
	return nil
}

// ---------------------------------------------------------------------------
// org revoke-console / restore-console — the console's credential, cut off and
// brought back
// ---------------------------------------------------------------------------

// atlDBFlag registers the flag naming ONE ORGANISATION'S atlantis database.
//
// Not Cloud's, and not the console's. Both of those are control-plane databases
// shared across the fleet; this is the per-organisation one that holds
// atlantis.caller_identities, and pointing this at either of the others finds no
// such table and stops.
//
// There is no default and no environment variable on purpose. Cloud does not
// know this address — the provisioner builds it from the CNPG cluster it created
// and passes it to the workload as PG_URL, and it is never written back. A
// default would therefore have to be wrong for every organisation but one, and
// the failure mode of a half-right default here is revoking the console's access
// to an organisation the operator was not looking at.
func atlDBFlag(fs *flag.FlagSet) *string {
	return fs.String("atl-db", "",
		"this organisation's atlantis database URL (from its namespace's PG_URL)")
}

// openOrgAtlantis connects to one organisation's atlantis database and proves
// the connection before returning it.
//
// pgxpool.New is lazy, so a pool built from an unreachable or misspelt DSN is
// returned happily and fails at the first query — which here would report a
// problem with caller_identities rather than with the address the operator
// typed. The same lesson cmd/signer learned when PG_URL became required.
func openOrgAtlantis(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open the organisation's atlantis database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to the organisation's atlantis database: %w", err)
	}
	return pool, nil
}

// consoleRevocationState reports whether the console has an identity row in this
// database and whether it is currently revoked.
func consoleRevocationState(ctx context.Context, pool *pgxpool.Pool) (exists, revoked bool, err error) {
	err = pool.QueryRow(ctx, `
SELECT revoked_at IS NOT NULL FROM atlantis.caller_identities WHERE caller = $1`,
		certs.ConsoleCN).Scan(&revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("read the console's identity: %w", err)
	}
	return true, revoked, nil
}

// orgRevokeConsole cuts the console off from one organisation.
//
// # Why this exists as a command rather than a console button
//
// The console is the thing being revoked. A button would work exactly once and
// then remove the operator's ability to press anything else in that
// organisation, and the way back is this command's opposite — which cannot be a
// button either, for the same reason. Both halves belong somewhere the console
// is not.
//
// # What it does and does not reach
//
// One organisation. The console holds a separate credential per organisation,
// so this cuts off the one whose database is named and leaves the rest serving.
// That is usually what an incident wants; revoking the fleet means running it
// per organisation, deliberately.
func orgRevokeConsole(args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("org revoke-console", flag.ExitOnError)
	name := fs.String("org", "", "organisation name; used in the printed record")
	yes := fs.Bool("yes", false, "required: confirm that this cuts the console off from this organisation")
	atlDB := atlDBFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return errors.New("-org is required")
	}
	if *atlDB == "" {
		return errors.New("-atl-db is required: the organisation's atlantis database URL")
	}
	if !*yes {
		return fmt.Errorf("refusing to revoke the console for %s without -yes: nobody "+
			"can open this organisation in a browser until it is restored, and the "+
			"restore path is a command rather than a button", *name)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := openOrgAtlantis(ctx, *atlDB)
	if err != nil {
		return err
	}
	defer pool.Close()

	// Read first, so the operator is told the state they are acting on. A
	// database that has no console row at all is a misconfigured organisation
	// rather than a revocable one, and saying "revoked" there would be a lie.
	exists, revoked, err := consoleRevocationState(ctx, pool)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%s has no %q identity, so there is nothing to revoke — "+
			"migration 0019 seeds it, so this database is either not an atlantis "+
			"database or has not been migrated", *name, certs.ConsoleCN)
	}
	if revoked {
		fmt.Printf("the console is already revoked for %s; nothing to do\n", *name)
		return nil
	}

	if _, err := pool.Exec(ctx, `
UPDATE atlantis.caller_identities SET revoked_at = NOW() WHERE caller = $1`,
		certs.ConsoleCN); err != nil {
		return fmt.Errorf("revoke the console: %w", err)
	}
	log.Info("revoked the console", "org", *name, "caller", certs.ConsoleCN)

	fmt.Printf("revoking the console for %s\n", *name)
	fmt.Println()
	fmt.Println("    Its capability grants are kept, so restoring is one command and")
	fmt.Println("    does not have to rebuild them:")
	fmt.Printf("      cloud org restore-console -org %s -atl-db ...\n", *name)
	fmt.Println()
	fmt.Println("    The server stops accepting the console within five seconds — the")
	fmt.Println("    cert-binding cache's TTL. Sessions already open start failing then.")
	return nil
}

// orgRestoreConsole brings the console back for one organisation.
//
// # Why this cannot go through RegisterCaller
//
// RegisterCaller refuses 'atlantis-console' as a reserved name, and refuses a
// revoked caller besides. That is deliberate in both cases, and it is exactly
// what made revocation a one-way door before migration 0033: with the identity
// row deleted, no supported command could recreate it. Marking the row instead
// of deleting it is what turns the way back into clearing one column.
func orgRestoreConsole(args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("org restore-console", flag.ExitOnError)
	name := fs.String("org", "", "organisation name; used in the printed record")
	atlDB := atlDBFlag(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" {
		return errors.New("-org is required")
	}
	if *atlDB == "" {
		return errors.New("-atl-db is required: the organisation's atlantis database URL")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := openOrgAtlantis(ctx, *atlDB)
	if err != nil {
		return err
	}
	defer pool.Close()

	exists, revoked, err := consoleRevocationState(ctx, pool)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%s has no %q identity to restore — migration 0019 seeds "+
			"it, so this database is either not an atlantis database or has not been "+
			"migrated", *name, certs.ConsoleCN)
	}
	// Reported rather than silently succeeding. An operator who runs this
	// because the console is unreachable needs to learn that revocation was not
	// the reason, instead of being told the problem is fixed.
	if !revoked {
		fmt.Printf("the console is not revoked for %s; nothing to do\n", *name)
		fmt.Println()
		fmt.Println("    If the console cannot reach this organisation, revocation is not")
		fmt.Println("    why. Check its certificate and the endpoint in console.orgs.")
		return nil
	}

	if _, err := pool.Exec(ctx, `
UPDATE atlantis.caller_identities SET revoked_at = NULL WHERE caller = $1`,
		certs.ConsoleCN); err != nil {
		return fmt.Errorf("restore the console: %w", err)
	}
	log.Info("restored the console", "org", *name, "caller", certs.ConsoleCN)

	fmt.Printf("restored the console for %s, with its capability grants intact\n", *name)
	fmt.Println()
	fmt.Println("    The server accepts it again within five seconds.")
	return nil
}
