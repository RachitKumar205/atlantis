package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rachitkumar205/atlantis/internal/cloud/authn"
	"github.com/rachitkumar205/atlantis/internal/cloud/issuer"
	"github.com/rachitkumar205/atlantis/internal/cloud/store"
	"github.com/rachitkumar205/atlantis/internal/testsupport/pgcatalog"
)

// Cloud's account routes, against a real database.
//
// The mailer and the breach corpus are recorded rather than real: one would
// send email and the other would reach a third party, and neither is what these
// tests are about. The database is real, because every property here is about
// what a row does.

type sentMessage struct{ To, Subject, Body string }

type recordingMailer struct {
	mu   sync.Mutex
	sent []sentMessage
	err  error
}

func (m *recordingMailer) Send(_ context.Context, to, subject, body string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.sent = append(m.sent, sentMessage{to, subject, body})
	return nil
}

func (m *recordingMailer) messages() []sentMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]sentMessage(nil), m.sent...)
}

// lastLink pulls the URL out of the most recent message, which is what a user
// would click.
func (m *recordingMailer) lastToken(t *testing.T) string {
	t.Helper()
	msgs := m.messages()
	if len(msgs) == 0 {
		t.Fatal("no message was sent")
	}
	body := msgs[len(msgs)-1].Body
	i := strings.Index(body, "token=")
	if i < 0 {
		t.Fatalf("no token in the message body: %s", body)
	}
	token := body[i+len("token="):]
	if j := strings.IndexAny(token, "\n \t"); j >= 0 {
		token = token[:j]
	}
	return token
}

type stubBreach struct {
	breached bool
	err      error
}

func (s stubBreach) Breached(context.Context, string) (bool, error) { return s.breached, s.err }

type fixture struct {
	srv    *Server
	db     *store.Store
	mailer *recordingMailer

	// slept records what the latency floor asked for instead of spending it.
	mu    sync.Mutex
	slept []time.Duration
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	adminDSN := os.Getenv("ATLANTIS_TEST_PG")
	if adminDSN == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise Cloud's account routes")
	}

	dsn := pgcatalog.PrivateDatabase(t, adminDSN, "atlantis_cloud_server")
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := store.Migrate(dsn, quiet); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	db, err := store.New(context.Background(), dsn, quiet)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(db.Close)

	key, err := issuer.GenerateKey()
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	iss, err := issuer.New("https://cloud.test", key)
	if err != nil {
		t.Fatalf("issuer: %v", err)
	}

	f := &fixture{db: db, mailer: &recordingMailer{}}
	f.srv = New(Config{
		PublicURL:     "https://cloud.test",
		CheckBreaches: false,
	}, db, iss, quiet)
	t.Cleanup(f.srv.Close)

	f.srv.mailer = f.mailer
	f.srv.breach = stubBreach{}
	f.srv.sleep = func(_ context.Context, d time.Duration) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.slept = append(f.slept, d)
	}
	return f
}

func (f *fixture) sleeps() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.slept...)
}

func (f *fixture) post(t *testing.T, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	return f.postFrom(t, "203.0.113.9", path, body)
}

// postFrom sends from a chosen address, for tests that make more requests than
// the rate limiter allows one client and are not about the rate limiter.
func (f *fixture) postFrom(t *testing.T, ip, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.RemoteAddr = ip + ":1234"
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, r)
	return rec
}

func (f *fixture) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.RemoteAddr = "203.0.113.9:1234"
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, r)
	return rec
}

const goodPassword = "unrelated-marmalade-turbine-9"

// Signing up creates an account and sends a verification link.
func TestSignupCreatesAnUnverifiedAccount(t *testing.T) {
	f := newFixture(t)

	rec := f.post(t, "/api/auth/signup",
		`{"email":"ada@example.com","password":"`+goodPassword+`","name":"Ada"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	u, err := f.db.UserByEmail(context.Background(), "ada@example.com")
	if err != nil {
		t.Fatalf("the account was not created: %v", err)
	}
	if !u.HasPassword() {
		t.Error("the account has no password")
	}
	// Unverified until the link is followed. An account that arrives verified
	// makes the email pointless.
	if u.EmailVerifiedAt != nil {
		t.Error("the account is verified before the link was followed")
	}

	msgs := f.mailer.messages()
	if len(msgs) != 1 {
		t.Fatalf("sent %d messages, want 1", len(msgs))
	}
	if msgs[0].To != "ada@example.com" {
		t.Errorf("sent to %q", msgs[0].To)
	}
	if !strings.Contains(msgs[0].Body, "https://cloud.test/verify?token=") {
		t.Errorf("the message has no verification link: %s", msgs[0].Body)
	}
	// The password must not be anywhere near an email.
	if strings.Contains(msgs[0].Body, goodPassword) {
		t.Error("the password is in the email")
	}
}

// Signing up with an address that already has an account is indistinguishable
// from signing up with a new one.
//
// This route takes an address chosen by whoever is asking. If the two paths
// differ, it is a membership oracle — point it at a list and learn who has an
// Atlantis Cloud account, which is to say whose credentials are worth stealing.
func TestSignupDoesNotRevealAnExistingAccount(t *testing.T) {
	f := newFixture(t)

	first := f.post(t, "/api/auth/signup",
		`{"email":"taken@example.com","password":"`+goodPassword+`","name":""}`)
	second := f.post(t, "/api/auth/signup",
		`{"email":"taken@example.com","password":"`+goodPassword+`","name":""}`)

	if first.Code != second.Code {
		t.Errorf("status differs: %d then %d", first.Code, second.Code)
	}
	if first.Body.String() != second.Body.String() {
		t.Errorf("body differs:\n  %s\n  %s", first.Body.String(), second.Body.String())
	}

	// And no second account was created.
	var n int
	if err := f.db.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM cloud.users WHERE email = 'taken@example.com'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("%d accounts exist for one address", n)
	}

	// The owner of the address is told, which is the point of answering the
	// same way to the requester: the person who can actually read the mailbox
	// finds out, and the person who asked does not.
	msgs := f.mailer.messages()
	if len(msgs) != 2 {
		t.Fatalf("sent %d messages, want 2", len(msgs))
	}
	if msgs[1].Subject == msgs[0].Subject {
		t.Error("the second attempt sent the same message as the first, so it " +
			"either created a second account or told nobody")
	}
	if !strings.Contains(strings.ToLower(msgs[1].Subject), "tried to sign up") {
		t.Errorf("the owner was not told what happened: %q", msgs[1].Subject)
	}
}

// Both paths are held to the same latency floor.
//
// The bodies being identical is not enough on its own. The registered path
// writes a row, mints a token and sends a message; the unregistered one does
// none of that. The difference is tens of milliseconds and is measurable from
// outside, so the body says nothing and the clock says everything.
func TestSignupAndResetAreHeldToALatencyFloor(t *testing.T) {
	f := newFixture(t)

	f.post(t, "/api/auth/signup", `{"email":"known@example.com","password":"`+goodPassword+`","name":""}`)
	f.post(t, "/api/auth/reset/request", `{"email":"known@example.com"}`)
	f.post(t, "/api/auth/reset/request", `{"email":"unknown@example.com"}`)

	sleeps := f.sleeps()
	if len(sleeps) != 3 {
		t.Fatalf("the floor was applied %d times across 3 requests, want 3", len(sleeps))
	}
	for i, d := range sleeps {
		if d <= 0 {
			t.Errorf("request %d waited %v, so the floor did not apply to it", i, d)
		}
	}

	// The floor must exceed the slow path, or the slow path pokes through it
	// and the defence silently stops working for exactly the requests that
	// matter. Asserted as a property of the constant rather than measured,
	// because measuring it here would make the test a benchmark.
	if latencyFloor < 250*time.Millisecond {
		t.Errorf("the floor is %v, which is under the cost of one argon2id hash — "+
			"the registered path would overrun it", latencyFloor)
	}
}

// A weak password is refused, and no account is created.
func TestSignupRefusesAWeakPassword(t *testing.T) {
	f := newFixture(t)

	rec := f.post(t, "/api/auth/signup", `{"email":"weak@example.com","password":"Passw0rd!","name":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if _, err := f.db.UserByEmail(context.Background(), "weak@example.com"); !errors.Is(err, store.ErrNotFound) {
		t.Error("an account was created despite the password being refused")
	}
	if len(f.mailer.messages()) != 0 {
		t.Error("a message was sent for a refused sign-up")
	}
}

// A breached password is refused; an unreachable corpus is not.
//
// The second half is the one worth pinning. Sign-up must not stop working
// because a third party is down, so the check fails open — and a test that only
// covered the refusal would pass against an implementation that failed closed
// and took the product down with the corpus.
func TestSignupHandlesTheBreachCorpus(t *testing.T) {
	t.Run("a known password is refused", func(t *testing.T) {
		f := newFixture(t)
		f.srv.breach = stubBreach{breached: true}

		rec := f.post(t, "/api/auth/signup",
			`{"email":"pwned@example.com","password":"`+goodPassword+`","name":""}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status %d, want 400", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "breach") {
			t.Errorf("the error does not say why: %s", rec.Body.String())
		}
	})

	t.Run("an unreachable corpus accepts the password", func(t *testing.T) {
		f := newFixture(t)
		f.srv.breach = stubBreach{err: errors.New("corpus unreachable")}

		rec := f.post(t, "/api/auth/signup",
			`{"email":"open@example.com","password":"`+goodPassword+`","name":""}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("a sign-up failed because the breach corpus was down: %d %s",
				rec.Code, rec.Body.String())
		}
		if _, err := f.db.UserByEmail(context.Background(), "open@example.com"); err != nil {
			t.Errorf("the account was not created: %v", err)
		}
	})
}

// A malformed address is refused before anything is written.
func TestSignupRefusesAMalformedAddress(t *testing.T) {
	f := newFixture(t)

	// One source address per case: six requests from one client would trip the
	// rate limiter partway through, and this test is not about the limiter.
	for i, addr := range []string{
		"not-an-address", "no@domain", "two@@example.com",
		"Ada <ada@example.com>", "ada@example.com\r\nBcc: x@y.z", "",
	} {
		ip := "198.51.100." + itoa(i+1)
		rec := f.postFrom(t, ip, "/api/auth/signup",
			`{"email":`+jsonString(addr)+`,"password":"`+goodPassword+`","name":""}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("accepted %q with status %d", addr, rec.Code)
		}
	}
	if len(f.mailer.messages()) != 0 {
		t.Error("a message was sent for a malformed address")
	}
}

// The verification link works, once.
func TestVerifyingAnAddress(t *testing.T) {
	f := newFixture(t)
	f.post(t, "/api/auth/signup", `{"email":"v@example.com","password":"`+goodPassword+`","name":""}`)
	token := f.mailer.lastToken(t)

	rec := f.get(t, "/verify?token="+url.QueryEscape(token))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	u, _ := f.db.UserByEmail(context.Background(), "v@example.com")
	if u.EmailVerifiedAt == nil {
		t.Fatal("the address is still unverified")
	}

	// A second click is refused rather than reporting success — the token is
	// spent, and pretending otherwise would hide a replay.
	if again := f.get(t, "/verify?token="+url.QueryEscape(token)); again.Code != http.StatusBadRequest {
		t.Errorf("a spent verification link answered %d, want 400", again.Code)
	}
	// An unknown token is refused the same way, so the two are not
	// distinguishable.
	if bogus := f.get(t, "/verify?token=nonsense"); bogus.Code != http.StatusBadRequest {
		t.Errorf("an unknown token answered %d, want 400", bogus.Code)
	}
}

// Requesting a reset answers the same for a known and an unknown address.
func TestResetRequestDoesNotRevealWhoHasAnAccount(t *testing.T) {
	f := newFixture(t)
	f.post(t, "/api/auth/signup", `{"email":"real@example.com","password":"`+goodPassword+`","name":""}`)

	known := f.post(t, "/api/auth/reset/request", `{"email":"real@example.com"}`)
	unknown := f.post(t, "/api/auth/reset/request", `{"email":"ghost@example.com"}`)
	malformed := f.post(t, "/api/auth/reset/request", `{"email":"not-an-address"}`)

	for _, rec := range []*httptest.ResponseRecorder{unknown, malformed} {
		if rec.Code != known.Code || rec.Body.String() != known.Body.String() {
			t.Errorf("a response differs from the known-address one:\n  %d %s\n  %d %s",
				known.Code, known.Body.String(), rec.Code, rec.Body.String())
		}
	}

	// One message, for the address that exists. The others sent nothing, which
	// is why the timing floor above carries the weight.
	msgs := f.mailer.messages()
	resets := 0
	for _, m := range msgs {
		if strings.Contains(m.Body, "/reset?token=") {
			resets++
		}
	}
	if resets != 1 {
		t.Errorf("sent %d reset links, want 1", resets)
	}
}

// A reset sets the password and does not sign anybody in.
func TestCompletingAReset(t *testing.T) {
	f := newFixture(t)
	f.post(t, "/api/auth/signup", `{"email":"r@example.com","password":"`+goodPassword+`","name":""}`)
	before, _ := f.db.UserByEmail(context.Background(), "r@example.com")

	f.post(t, "/api/auth/reset/request", `{"email":"r@example.com"}`)
	token := f.mailer.lastToken(t)

	const newPassword = "entirely-different-quiet-lantern-4"
	rec := f.post(t, "/api/auth/reset/complete",
		`{"token":`+jsonString(token)+`,"password":"`+newPassword+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	after, _ := f.db.UserByEmail(context.Background(), "r@example.com")
	if *after.PasswordHash == *before.PasswordHash {
		t.Fatal("the password did not change")
	}

	// No session, no cookie. Proving control of a mailbox is one factor;
	// signing in needs two, so a reset that produced a session would make the
	// mailbox sufficient on its own.
	if cookies := rec.Result().Cookies(); len(cookies) != 0 {
		t.Errorf("completing a reset set %d cookies", len(cookies))
	}

	// And the link is spent.
	if again := f.post(t, "/api/auth/reset/complete",
		`{"token":`+jsonString(token)+`,"password":"`+newPassword+`"}`); again.Code != http.StatusBadRequest {
		t.Errorf("a spent reset link answered %d, want 400", again.Code)
	}
}

// A rejected password still spends the link.
//
// Otherwise one link is an unlimited oracle against the strength check: try
// candidate passwords until one is accepted, learning the rules for free.
func TestARejectedPasswordStillSpendsTheLink(t *testing.T) {
	f := newFixture(t)
	f.post(t, "/api/auth/signup", `{"email":"grind@example.com","password":"`+goodPassword+`","name":""}`)
	f.post(t, "/api/auth/reset/request", `{"email":"grind@example.com"}`)
	token := f.mailer.lastToken(t)

	weak := f.post(t, "/api/auth/reset/complete", `{"token":`+jsonString(token)+`,"password":"short"}`)
	if weak.Code != http.StatusBadRequest {
		t.Fatalf("a weak password was accepted: %d", weak.Code)
	}
	// Said plainly, because the user now has to ask for another link and would
	// otherwise keep trying this one.
	if !strings.Contains(weak.Body.String(), "request another") {
		t.Errorf("the error does not say the link is gone: %s", weak.Body.String())
	}

	good := f.post(t, "/api/auth/reset/complete",
		`{"token":`+jsonString(token)+`,"password":"a-second-attempt-passphrase-7"}`)
	if good.Code != http.StatusBadRequest {
		t.Error("the link survived a rejected password, so it is an unlimited " +
			"oracle against the strength check")
	}
}

// The emailed form works end to end, form encoding and all.
//
// The reset email links to a page a person opens in a browser. That page posts
// form-encoded data, not JSON — so a route that only accepted JSON would leave
// every emailed link dead, which is the defect this test exists to catch. It
// caught it once already.
func TestTheEmailedResetFormWorks(t *testing.T) {
	f := newFixture(t)
	f.post(t, "/api/auth/signup", `{"email":"form@example.com","password":"`+goodPassword+`","name":""}`)
	f.post(t, "/api/auth/reset/request", `{"email":"form@example.com"}`)
	token := f.mailer.lastToken(t)

	// The page renders and carries the token.
	page := f.get(t, "/reset?token="+url.QueryEscape(token))
	if page.Code != http.StatusOK {
		t.Fatalf("the reset page answered %d", page.Code)
	}
	if !strings.Contains(page.Body.String(), token) {
		t.Error("the form does not carry the token, so submitting it cannot work")
	}

	// And submitting it changes the password.
	form := url.Values{"token": {token}, "password": {"a-brand-new-quiet-lantern-8"}}
	r := httptest.NewRequest(http.MethodPost, "/reset", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.RemoteAddr = "203.0.113.9:1234"
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("submitting the form answered %d: %s", rec.Code, rec.Body.String())
	}
	u, _ := f.db.UserByEmail(context.Background(), "form@example.com")
	ok, _, err := verifyFor(t, "a-brand-new-quiet-lantern-8", *u.PasswordHash)
	if err != nil || !ok {
		t.Error("the password submitted through the form was not the one stored")
	}
}

// The token is escaped where it lands in the page.
//
// It arrives in the query string, so it is attacker-supplied, and it is placed
// into an HTML attribute — the textbook injection point.
func TestTheResetPageEscapesTheToken(t *testing.T) {
	f := newFixture(t)

	rec := f.get(t, "/reset?token="+url.QueryEscape(`"><script>alert(1)</script>`))
	body := rec.Body.String()
	if strings.Contains(body, "<script>") {
		t.Fatalf("the token was written into the page unescaped:\n%s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("the token does not appear escaped either, so it may have been dropped:\n%s", body)
	}
}

// Too many requests from one address are refused.
//
// Sign-up and reset both cause a message to be sent to an address the requester
// chose. Without a limit that is a mail cannon pointed at anyone, spending our
// sending reputation.
func TestRequestsAreRateLimited(t *testing.T) {
	f := newFixture(t)

	var lastCode int
	for i := range limiterMax + 2 {
		rec := f.post(t, "/api/auth/reset/request", `{"email":"rl@example.com"}`)
		lastCode = rec.Code
		if i < limiterMax && rec.Code != http.StatusOK {
			t.Fatalf("request %d was refused with %d before the limit", i, rec.Code)
		}
	}
	if lastCode != http.StatusTooManyRequests {
		t.Fatalf("the %dth request answered %d, want 429", limiterMax+2, lastCode)
	}
}

// Every response carries the headers that keep a token out of a referrer.
func TestSecurityHeaders(t *testing.T) {
	f := newFixture(t)
	rec := f.get(t, "/healthz")

	// These pages are reached with a single-use token in the query string. The
	// default referrer policy would put that token in the Referer of anything
	// the page loads.
	if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy is %q", got)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("Content-Security-Policy is %q", csp)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options is %q", got)
	}
}

// The key set is still served, from the same path consoles already fetch.
func TestTheKeySetIsStillPublished(t *testing.T) {
	f := newFixture(t)

	rec := f.get(t, issuer.JWKSPath)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var jwks struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &jwks); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(jwks.Keys) == 0 {
		t.Fatal("the key set is empty, so no console could verify anything")
	}
	// The private half must not be in it. The issuer package has its own test
	// for this; repeated here because this is the surface actually exposed.
	if strings.Contains(rec.Body.String(), `"d"`) {
		t.Error("the published key set contains a private key component")
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// verifyFor checks a password against a stored hash, so a test can assert the
// hash the routes wrote is the one for the password that was submitted.
func verifyFor(t *testing.T, password, hash string) (bool, bool, error) {
	t.Helper()
	return authn.Verify(password, hash)
}
