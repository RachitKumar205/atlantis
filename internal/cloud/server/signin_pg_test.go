package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rachitkumar205/atlantis/internal/cloud/authn"
	"github.com/rachitkumar205/atlantis/internal/cloud/store"
)

// Signing in, and the second factor that gates it.

// verifiedAccount signs somebody up and follows the verification link, leaving
// an account that can sign in but has no second factor.
func (f *fixture) verifiedAccount(t *testing.T, email string) *store.User {
	t.Helper()
	rec := f.post(t, "/api/auth/signup",
		`{"email":`+jsonString(email)+`,"password":"`+goodPassword+`","name":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("signup: %d %s", rec.Code, rec.Body.String())
	}
	token := f.mailer.lastToken(t)
	if v := f.get(t, "/verify?token="+token); v.Code != http.StatusOK {
		t.Fatalf("verify: %d %s", v.Code, v.Body.String())
	}
	u, err := f.db.UserByEmail(context.Background(), email)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	return u
}

// cookie pulls a named cookie off a response.
func cookieFrom(rec *httptest.ResponseRecorder, name string) string {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name && c.Value != "" {
			return c.Value
		}
	}
	return ""
}

// postWithCookie sends a request carrying one cookie.
func (f *fixture) postWithCookie(t *testing.T, path, body, name, value string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.RemoteAddr = f.nextIP() + ":1234"
	if value != "" {
		r.AddCookie(&http.Cookie{Name: name, Value: value})
	}
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, r)
	return rec
}

// enrol takes an account from verified to signed-in with a second factor,
// returning the session cookie and the backup codes it was issued.
func (f *fixture) enrol(t *testing.T, email string) (session, secret string, codes []string) {
	t.Helper()

	login := f.post(t, "/api/auth/login",
		`{"email":`+jsonString(email)+`,"password":"`+goodPassword+`"}`)
	if login.Code != http.StatusOK {
		t.Fatalf("login: %d %s", login.Code, login.Body.String())
	}
	pending := cookieFrom(login, pendingCookie)
	if pending == "" {
		t.Fatal("login set no pending cookie")
	}

	begin := f.postWithCookie(t, "/api/auth/2fa/enrol/begin", `{}`, pendingCookie, pending)
	if begin.Code != http.StatusOK {
		t.Fatalf("enrol begin: %d %s", begin.Code, begin.Body.String())
	}
	var started struct{ Secret string }
	if err := json.Unmarshal(begin.Body.Bytes(), &started); err != nil {
		t.Fatalf("decode: %v", err)
	}

	code := codeAt(t, started.Secret, time.Now())
	finish := f.postWithCookie(t, "/api/auth/2fa/enrol/finish",
		`{"code":`+jsonString(code)+`}`, pendingCookie, pending)
	if finish.Code != http.StatusOK {
		t.Fatalf("enrol finish: %d %s", finish.Code, finish.Body.String())
	}
	var done struct {
		BackupCodes []string `json:"backup_codes"`
	}
	if err := json.Unmarshal(finish.Body.Bytes(), &done); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return cookieFrom(finish, sessionCookie), started.Secret, done.BackupCodes
}

// codeAt computes the code an authenticator would show at a moment.
//
// Through the same package the server verifies with, so the fixture cannot
// disagree with the implementation about period or digits.
func codeAt(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	code, err := authn.CodeAt(secret, at)
	if err != nil {
		t.Fatalf("compute code: %v", err)
	}
	return code
}

// enrol returns the secret the server handed out, rather than reading it back
// out of the database.
//
// There was a helper here that did the latter — it decrypted the stored
// ciphertext with the user id as associated data, exactly as the server does.
// Mutation testing showed the cost: changing that associated data to a constant
// broke four tests, all of them inside this helper, including the one named for
// the property. A test that fails in a helper does not report which property
// broke, and the one test that should have failed on its own terms never
// reached its assertion.
//
// Taking the secret from the enrolment response removes the coupling. Every
// test below now exercises the seal only through the routes a user goes
// through, which is the only path that proves anything about it.

// A password alone does not produce a session.
//
// This is the property the whole two-table split exists for. If login ever
// returns a session cookie, one factor is enough and the second is decoration.
func TestAPasswordAloneIsNotASession(t *testing.T) {
	f := newFixture(t)
	f.verifiedAccount(t, "one@example.com")

	rec := f.post(t, "/api/auth/login", `{"email":"one@example.com","password":"`+goodPassword+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}
	if s := cookieFrom(rec, sessionCookie); s != "" {
		t.Fatal("a password alone produced a session cookie")
	}
	pending := cookieFrom(rec, pendingCookie)
	if pending == "" {
		t.Fatal("login set no pending cookie, so the sign-in cannot continue")
	}

	// And the pending token is not a session: presented as one, nothing accepts
	// it. That is what the separate table buys, so it is asserted rather than
	// assumed.
	if _, err := f.db.SessionUser(context.Background(), pending); err == nil {
		t.Fatal("a pending login resolved as a session — the two tables are not " +
			"separating what they were split to separate")
	}
}

// A wrong password is refused, and so is an unknown address, identically.
func TestSignInRefusalsAreIndistinguishable(t *testing.T) {
	f := newFixture(t)
	f.verifiedAccount(t, "real@example.com")

	wrong := f.post(t, "/api/auth/login", `{"email":"real@example.com","password":"not-the-password-at-all"}`)
	unknown := f.postFrom(t, "198.51.100.7", "/api/auth/login",
		`{"email":"ghost@example.com","password":"not-the-password-at-all"}`)

	if wrong.Code != http.StatusUnauthorized || unknown.Code != http.StatusUnauthorized {
		t.Fatalf("statuses: wrong=%d unknown=%d, want 401 for both", wrong.Code, unknown.Code)
	}
	if wrong.Body.String() != unknown.Body.String() {
		t.Errorf("bodies differ:\n  %s\n  %s", wrong.Body.String(), unknown.Body.String())
	}
	// The floor applies to both, which is what stops the clock saying what the
	// body does not — the unknown branch does no database write and would
	// otherwise return measurably sooner.
	if n := len(f.sleeps()); n < 2 {
		t.Errorf("the latency floor applied %d times across two refusals", n)
	}
}

// An unverified address cannot sign in, and can ask for another link.
func TestAnUnverifiedAddressCannotSignIn(t *testing.T) {
	f := newFixture(t)
	f.post(t, "/api/auth/signup", `{"email":"unv@example.com","password":"`+goodPassword+`","name":""}`)

	rec := f.post(t, "/api/auth/login", `{"email":"unv@example.com","password":"`+goodPassword+`"}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if cookieFrom(rec, pendingCookie) != "" {
		t.Error("an unverified account got a pending login")
	}

	before := len(f.mailer.messages())
	resend := f.postFrom(t, "198.51.100.8", "/api/auth/verify/resend", `{"email":"unv@example.com"}`)
	if resend.Code != http.StatusOK {
		t.Fatalf("resend: %d", resend.Code)
	}
	if len(f.mailer.messages()) != before+1 {
		t.Error("resend sent no message")
	}

	// And an unknown address gets the same answer, so resend is not a
	// membership oracle either.
	unknown := f.postFrom(t, "198.51.100.9", "/api/auth/verify/resend", `{"email":"nobody@example.com"}`)
	if unknown.Body.String() != resend.Body.String() {
		t.Errorf("resend distinguishes a known address:\n  %s\n  %s",
			resend.Body.String(), unknown.Body.String())
	}
}

// Enrolment turns a password into a session, and issues backup codes once.
func TestEnrolmentCompletesASignIn(t *testing.T) {
	f := newFixture(t)
	f.verifiedAccount(t, "enrol@example.com")

	session, _, codes := f.enrol(t, "enrol@example.com")
	if session == "" {
		t.Fatal("finishing enrolment did not sign the user in")
	}
	if len(codes) != authn.BackupCodeCount {
		t.Fatalf("got %d backup codes, want %d", len(codes), authn.BackupCodeCount)
	}

	u, err := f.db.SessionUser(context.Background(), session)
	if err != nil {
		t.Fatalf("the session does not resolve: %v", err)
	}
	if u.Email != "enrol@example.com" {
		t.Errorf("the session belongs to %s", u.Email)
	}
}

// An account that already has a factor cannot enrol from a pending login.
//
// Otherwise somebody holding only a password replaces the second factor with
// one of their own, which walks around the entire gate.
func TestAPasswordCannotReplaceAnExistingSecondFactor(t *testing.T) {
	f := newFixture(t)
	f.verifiedAccount(t, "settled@example.com")
	f.enrol(t, "settled@example.com")

	login := f.postFrom(t, "198.51.100.20", "/api/auth/login",
		`{"email":"settled@example.com","password":"`+goodPassword+`"}`)
	pending := cookieFrom(login, pendingCookie)
	if pending == "" {
		t.Fatal("no pending cookie")
	}

	rec := f.postWithCookie(t, "/api/auth/2fa/enrol/begin", `{}`, pendingCookie, pending)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("a password-only login reached enrolment: %d %s", rec.Code, rec.Body.String())
	}
}

// A TOTP code signs the user in, and cannot be presented twice.
//
// A code is valid for its whole period and the periods either side, up to
// ninety seconds. Within that window a code seen over a shoulder or relayed by
// a phishing proxy would otherwise work again.
func TestATOTPCodeIsSingleUse(t *testing.T) {
	f := newFixture(t)
	f.verifiedAccount(t, "totp@example.com")
	_, secret, _ := f.enrol(t, "totp@example.com")

	login := f.postFrom(t, "198.51.100.21", "/api/auth/login",
		`{"email":"totp@example.com","password":"`+goodPassword+`"}`)
	pending := cookieFrom(login, pendingCookie)

	// The enrolling code already spent the current step, so use the next one.
	// The server checks against its own clock with one step of skew either
	// side, so a code from step+1 is accepted now — and, being a later step,
	// passes the spend check that the enrolling code's step would fail.
	at := time.Now().Add(31 * time.Second)
	code := codeAt(t, secret, at)

	first := f.postWithCookie(t, "/api/auth/2fa/verify",
		`{"code":`+jsonString(code)+`}`, pendingCookie, pending)
	if first.Code != http.StatusOK {
		t.Fatalf("a valid code was refused: %d %s", first.Code, first.Body.String())
	}
	if cookieFrom(first, sessionCookie) == "" {
		t.Fatal("verifying the second factor produced no session")
	}

	// Same code, same window, second login. Refused.
	login2 := f.postFrom(t, "198.51.100.22", "/api/auth/login",
		`{"email":"totp@example.com","password":"`+goodPassword+`"}`)
	pending2 := cookieFrom(login2, pendingCookie)

	replay := f.postWithCookie(t, "/api/auth/2fa/verify",
		`{"code":`+jsonString(code)+`}`, pendingCookie, pending2)
	if replay.Code == http.StatusOK {
		t.Fatal("the same TOTP code was accepted twice inside its window")
	}
	if cookieFrom(replay, sessionCookie) != "" {
		t.Fatal("a replayed code produced a session")
	}
}

// A backup code works once, and the others survive.
func TestBackupCodesAreSingleUse(t *testing.T) {
	f := newFixture(t)
	f.verifiedAccount(t, "backup@example.com")
	_, _, codes := f.enrol(t, "backup@example.com")

	signInWith := func(ip, code string) *httptest.ResponseRecorder {
		login := f.postFrom(t, ip, "/api/auth/login",
			`{"email":"backup@example.com","password":"`+goodPassword+`"}`)
		return f.postWithCookie(t, "/api/auth/2fa/verify",
			`{"code":`+jsonString(code)+`}`, pendingCookie, cookieFrom(login, pendingCookie))
	}

	first := signInWith("198.51.100.30", codes[0])
	if first.Code != http.StatusOK {
		t.Fatalf("a backup code was refused: %d %s", first.Code, first.Body.String())
	}

	again := signInWith("198.51.100.31", codes[0])
	if again.Code == http.StatusOK {
		t.Fatal("the same backup code worked twice")
	}

	// A different code still works, so spending one did not spend them all.
	other := signInWith("198.51.100.32", codes[1])
	if other.Code != http.StatusOK {
		t.Fatalf("a second backup code was refused: %d %s", other.Code, other.Body.String())
	}

	// And typing one without its hyphen works, because the grouping is display.
	third := signInWith("198.51.100.33", strings.ReplaceAll(codes[2], "-", ""))
	if third.Code != http.StatusOK {
		t.Errorf("a backup code typed without its hyphen was refused: %s", third.Body.String())
	}
}

// A wrong code does not cost the user their pending login.
//
// Spending it on a failure would mean one mistyped digit sends them back to the
// password form. The rate limiter is what bounds guessing.
func TestAWrongCodeKeepsThePendingLogin(t *testing.T) {
	f := newFixture(t)
	f.verifiedAccount(t, "typo@example.com")
	_, secret, _ := f.enrol(t, "typo@example.com")

	login := f.postFrom(t, "198.51.100.40", "/api/auth/login",
		`{"email":"typo@example.com","password":"`+goodPassword+`"}`)
	pending := cookieFrom(login, pendingCookie)

	bad := f.postWithCookie(t, "/api/auth/2fa/verify", `{"code":"000000"}`, pendingCookie, pending)
	if bad.Code == http.StatusOK {
		t.Fatal("000000 was accepted")
	}

	at := time.Now().Add(31 * time.Second)
	good := f.postWithCookie(t, "/api/auth/2fa/verify",
		`{"code":`+jsonString(codeAt(t, secret, at))+`}`, pendingCookie, pending)
	if good.Code != http.StatusOK {
		t.Fatalf("the pending login did not survive one wrong code: %d %s",
			good.Code, good.Body.String())
	}
}

// The TOTP secret is not in the database in the clear.
func TestTOTPSecretsAreEncryptedAtRest(t *testing.T) {
	f := newFixture(t)
	u := f.verifiedAccount(t, "sealed@example.com")
	_, secret, _ := f.enrol(t, "sealed@example.com")

	// Read through the bound handle, because the table is policed and an
	// unbound SELECT returns nothing — the same trap that produced a real
	// second-factor bypass in this package.
	ct, _, err := f.db.ForUser(u.ID).TOTPSecret(context.Background())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(ct) == 0 {
		t.Fatal("the column is empty, so this test would pass against a Cloud " +
			"that stored no secret at all")
	}
	if strings.Contains(string(ct), secret) {
		t.Fatal("the TOTP secret is in the database in the clear — a leaked " +
			"backup would hand over every account's second factor")
	}
}

// A sealed secret cannot be moved onto another account.
//
// The associated data is the user id, which turns "somebody can UPDATE this
// table" from a way to graft a known second factor onto a victim's account into
// nothing.
func TestASealedSecretCannotBeMovedBetweenAccounts(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	mine := f.verifiedAccount(t, "mine@example.com")
	_, mineSecret, _ := f.enrol(t, "mine@example.com")
	theirs := f.verifiedAccount(t, "theirs@example.com")
	f.enrol(t, "theirs@example.com")

	// The move has to be made bound, one row at a time.
	//
	// This was a single unbound UPDATE with a subquery, and it moved nothing:
	// cloud.totp_secrets is policed, an unbound statement matches no rows, and
	// Exec returned no error because zero rows updated is a successful UPDATE.
	// The test then passed for the reason that nothing had happened — it would
	// have passed with the associated data removed entirely, which is the one
	// thing it exists to catch.
	//
	// It is the same shape as the bypass this package already shipped and
	// fixed, arriving through the fixture instead of through the code. Hence
	// the row count below: the setup now fails loudly if it silently no-ops.
	mineCT, _, err := f.db.ForUser(mine.ID).TOTPSecret(ctx)
	if err != nil {
		t.Fatalf("read my ciphertext: %v", err)
	}

	tx, err := f.db.Pool().Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Bound as the victim, which is what makes their row writable. The bind is
	// transaction-local, so it goes back with the commit.
	if _, err := tx.Exec(ctx, `SELECT cloud.set_user($1)`, theirs.ID); err != nil {
		t.Fatalf("bind: %v", err)
	}
	tag, err := tx.Exec(ctx,
		`UPDATE cloud.totp_secrets SET secret_ct = $2 WHERE user_id = $1`,
		theirs.ID, mineCT)
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("the move updated %d rows, so this test would prove nothing",
			tag.RowsAffected())
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// My code must not open their account.
	code := codeAt(t, mineSecret, time.Now().Add(31*time.Second))
	login := f.postFrom(t, "198.51.100.50", "/api/auth/login",
		`{"email":"theirs@example.com","password":"`+goodPassword+`"}`)
	rec := f.postWithCookie(t, "/api/auth/2fa/verify",
		`{"code":`+jsonString(code)+`}`, pendingCookie, cookieFrom(login, pendingCookie))

	if rec.Code == http.StatusOK {
		t.Fatal("a second factor lifted onto another account worked, so anyone " +
			"who can write this table can take over any account")
	}
}

// Signing out ends the session, twice over.
func TestSignOut(t *testing.T) {
	f := newFixture(t)
	f.verifiedAccount(t, "out@example.com")
	session, _, _ := f.enrol(t, "out@example.com")

	rec := f.postWithCookie(t, "/api/auth/logout", `{}`, sessionCookie, session)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout: %d", rec.Code)
	}
	if _, err := f.db.SessionUser(context.Background(), session); err == nil {
		t.Fatal("the session still resolves after signing out")
	}

	// A second sign-out is not an error. Browsers do this.
	again := f.postWithCookie(t, "/api/auth/logout", `{}`, sessionCookie, session)
	if again.Code != http.StatusOK {
		t.Errorf("a second sign-out answered %d", again.Code)
	}
}

// Enrolment cannot be reached without a password.
func TestEnrolmentRequiresAPassword(t *testing.T) {
	f := newFixture(t)

	for _, path := range []string{"/api/auth/2fa/enrol/begin", "/api/auth/2fa/enrol/finish"} {
		if rec := f.post(t, path, `{}`); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s answered %d with no credentials, want 401", path, rec.Code)
		}
	}
	if rec := f.post(t, "/api/auth/2fa/verify", `{"code":"123456"}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("2fa/verify answered %d with no pending login, want 401", rec.Code)
	}
}
