package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/rachitkumar205/atlantis/internal/analytics"
)

// Cloud's audit log. Provisioning creates a namespace, mints a certificate
// authority and writes an organisation's credentials, and those have to be
// answerable from the database rather than from log retention.

// ProvisionerActor is the actor recorded for work the provisioner does on its
// own initiative.
//
// A named constant, so a row the provisioner wrote is distinguishable from one
// that lost its actor. The human who caused it is traceable through the row
// their own action wrote.
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
// Returns nothing: an action that succeeded is not undone because recording it
// failed. A failed write is logged, so the gap in the log has something beside
// it saying why.
//
// The actor's email is written onto the row rather than resolved at read time,
// so an entry records who acted then and not who holds that identity now.
func (s *Store) LogAction(ctx context.Context, org, actor, actorEmail, action string, detail map[string]any) {
	var detailJSON []byte
	if detail != nil {
		b, err := json.Marshal(detail)
		if err != nil {
			// The action is recorded without its detail, rather than the row
			// being lost to an unmarshalable map.
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

	s.report(org, actor, action, detail)
}

// report emits the analytics event for an audited action.
//
// The projection decides what crosses. No path here copies detail through, so
// a key nobody named cannot leave the process.
//
// An action with no projection is a gap between the audit log and the
// catalogue; TestEveryCloudAuditActionIsProjected fails on one, and this warns
// in a deployment where the two have drifted anyway.
func (s *Store) report(org, actor, action string, detail map[string]any) {
	if s.sink == nil {
		return
	}
	p, ok := analytics.CloudActions[action]
	if !ok {
		s.log.Warn("audit action has no analytics projection", "action", action)
		return
	}
	if p.Event == "" {
		return
	}
	var props map[string]any
	if p.Props != nil {
		props = p.Props(detail)
	}
	s.sink.Capture(analytics.Event{
		Name:            p.Event,
		DistinctID:      distinctID(org, actor),
		Org:             org,
		Props:           props,
		NoPersonProfile: actor == ProvisionerActor,
	})
}

// distinctID is the actor for an event. Work the provisioner did on its own
// initiative has no person behind it and is attributed to the organisation.
func distinctID(org, actor string) string {
	if actor == ProvisionerActor || actor == "" {
		return analytics.MachineID(org)
	}
	return actor
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
			// A row whose detail will not decode still carries its action,
			// actor and time.
			if err := json.Unmarshal(detail, &e.Detail); err != nil {
				s.log.Warn("audit detail could not be decoded",
					"org", org, "id", e.ID, "err", err)
			}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
