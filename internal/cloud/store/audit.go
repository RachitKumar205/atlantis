package store

import (
	"context"
	"encoding/json"
	"time"
)

// ── The audit log ───────────────────────────────────────────────────────────
//
// Cloud has never had one. The console has, and Cloud's user id was designed to
// be its actor — cloud.users' own comment says the id becomes the `sub` claim
// "and therefore the audit actor" — but nothing on this side ever wrote a row.
// `cloud org register`, `cloud user create` and `cloud member add` leave no
// trace beyond the row they wrote and the line they printed.
//
// Provisioning is where that stops being tolerable. It creates a namespace,
// mints a certificate authority and writes an organisation's credentials, and
// "when was acme's CA minted, and by which provisioner" should be answerable
// from the database rather than from whatever log retention happens to exist.

// ProvisionerActor is the actor recorded for work the provisioner does on its
// own initiative.
//
// A named constant rather than an empty string, following the console's
// enrolmentActor: a blank actor reads as a bug in the logging rather than as a
// machine acting on its own behalf. The human who caused it is traceable
// through the paired row their own action wrote.
const ProvisionerActor = "provisioner"

// AuditEntry is one recorded action.
type AuditEntry struct {
	ID         int64
	Org        string
	Actor      string
	ActorEmail string
	Action     string
	Detail     map[string]any
	CreatedAt  time.Time
}

// LogAction records an action against an organisation.
//
// Returns nothing, deliberately: an action that succeeded is not undone because
// recording it failed. But the error is *logged* rather than discarded, which
// the console learned the hard way — its comment records months of a DELETE
// that matched nothing looking exactly like months with nothing to delete.
//
// The actor's email is written onto the row rather than resolved when the log
// is read. An entry should say who acted at the time it happened; a lookup
// reports whoever holds that identity now, which is a different claim and
// occasionally a false one.
func (s *Store) LogAction(ctx context.Context, org, actor, actorEmail, action string, detail map[string]any) {
	var detailJSON []byte
	if detail != nil {
		b, err := json.Marshal(detail)
		if err != nil {
			// Record the action without its detail rather than losing the
			// action. A detail map that will not marshal is a bug in the
			// caller, and it should not also cost us the evidence.
			s.log.Warn("audit detail could not be encoded",
				"org", org, "action", action, "err", err)
		} else {
			detailJSON = b
		}
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO cloud.audit_log (org, actor, actor_email, action, detail)
		VALUES ($1, $2, $3, $4, $5)
	`, org, actor, actorEmail, action, detailJSON)
	if err != nil {
		s.log.Warn("audit write failed",
			"org", org, "action", action, "actor", actor, "err", err)
	}
}

// AuditFor reads an organisation's most recent entries, newest first.
func (s *Store) AuditFor(ctx context.Context, org string, limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, org, actor, actor_email, action, detail, created_at
		  FROM cloud.audit_log WHERE org = $1
		 ORDER BY created_at DESC, id DESC
		 LIMIT $2
	`, org, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		var detail []byte
		if err := rows.Scan(&e.ID, &e.Org, &e.Actor, &e.ActorEmail,
			&e.Action, &detail, &e.CreatedAt); err != nil {
			return nil, err
		}
		if len(detail) > 0 {
			// A row whose detail will not decode is still a row worth
			// returning: the action, the actor and the time are the parts
			// somebody is asking about.
			if err := json.Unmarshal(detail, &e.Detail); err != nil {
				s.log.Warn("audit detail could not be decoded",
					"org", org, "id", e.ID, "err", err)
			}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
