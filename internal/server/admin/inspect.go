package admin

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

// InspectSchema reports how the live database differs from a declaration, and
// writes nothing.
//
// # Why this exists separately from AdoptBaseline
//
// Adopt answers two questions at once: "how do these differ" and "record that
// they agree". Operators were using `adopt --allow-drift` for the first one,
// which meant reaching for a flag whose name promises a write in order to ask
// a question. On a hosted deployment that is worse than awkward — the flag
// rewrites the shared checkpoint for every caller, so finding out required a
// privilege nobody should hold to look.
//
// This is the first question on its own, at CAPABILITY_SCHEMA_READ, so a
// caller can run it against production from CI.
//
// # How "writes nothing" is enforced
//
// The transaction is opened READ ONLY, so a write introduced on this path
// later fails at Postgres rather than in review. That matters because the
// path is SHARED with adopt: compareToLive is the same function both call,
// and the line after it in AdoptBaseline is persistCheckpoint. A guard that
// depends on nobody adding a write to a shared helper is not a guard.
//
// No advisory lock, unlike adopt. Adopt takes one because it rewrites the
// checkpoint and must serialise against apply. Reading does not, and taking
// the lock here would let a `tide inspect` in CI block a deploy.
func (s *Service) InspectSchema(ctx context.Context, req *adminpb.InspectSchemaRequest) (*adminpb.InspectSchemaResponse, error) {
	subs := callerSubmissionsFromPB(req.GetSubmissions())
	if len(subs) == 0 {
		if req.GetCaller() == "" || len(req.GetFiles()) == 0 {
			return nil, errors.New("admin: at least one CallerSubmission is required")
		}
		subs = []CallerSubmission{{Caller: req.GetCaller(), Files: submittedFilesFromPB(req.GetFiles())}}
	}
	for i, sub := range subs {
		if sub.Caller == "" {
			return nil, fmt.Errorf("admin: submission[%d]: caller is required", i)
		}
		if len(sub.Files) == 0 {
			return nil, fmt.Errorf("admin: submission[%d] %s: at least one .atl file is required", i, sub.Caller)
		}
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	// Rolled back, never committed. A READ ONLY transaction has nothing to
	// commit, and calling Commit would only invite somebody to wonder what it
	// was for.
	defer func() { _ = tx.Rollback(context.Background()) }()

	cmp, err := s.compareToLive(ctx, tx, subs)
	if err != nil {
		return nil, err
	}

	// in_sync is computed here rather than left to the client to infer from an
	// empty list. A CLI deciding an exit code must be able to tell "the
	// database matches the declaration" from "this build produced no findings
	// it knows how to name", and those look identical from the outside.
	return &adminpb.InspectSchemaResponse{
		Drift:         adoptDriftToPB(cmp.Drift),
		Warnings:      cmp.Warnings,
		InSync:        len(cmp.Drift) == 0,
		MismatchCount: int32(mismatchCount(cmp.Drift)),
	}, nil
}
