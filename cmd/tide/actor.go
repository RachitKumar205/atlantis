package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
)

// actorIdentity is the person a schema change is attributed to. The server
// records it as given, and refuses an approval from the same principal.
type actorIdentity struct {
	// ID is a scheme-qualified principal, such as console:<subject>.
	ID    string `json:"id"`
	Email string `json:"email,omitempty"`
	Name  string `json:"name,omitempty"`
}

// actorFromAssertion reads the Cloud account out of a sign-in assertion, or
// returns the zero identity when it has no subject.
//
// The claims are read without verifying the signature: the enrolment listener
// verifies the assertion, and the result is used only for attribution.
func actorFromAssertion(assertion string) actorIdentity {
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		return actorIdentity{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return actorIdentity{}
	}
	var claims struct {
		Sub   string `json:"sub"`
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Sub == "" {
		return actorIdentity{}
	}
	// The principal the console gives the same account, so the server's
	// self-approval refusal matches one person across tide and the console.
	return actorIdentity{ID: "console:" + claims.Sub, Email: claims.Email, Name: claims.Name}
}

// actor is ATL_ACTOR, ATL_ACTOR_EMAIL and ATL_ACTOR_NAME when ATL_ACTOR is
// set, and otherwise the account `tide login` signed in as. It is the zero
// identity for a login that named no person, such as an enrolment token.
func (c *tideConfig) actor() actorIdentity {
	if id := strings.TrimSpace(os.Getenv("ATL_ACTOR")); id != "" {
		return actorIdentity{
			ID:    id,
			Email: strings.TrimSpace(os.Getenv("ATL_ACTOR_EMAIL")),
			Name:  strings.TrimSpace(os.Getenv("ATL_ACTOR_NAME")),
		}
	}
	return c.storeActor
}
