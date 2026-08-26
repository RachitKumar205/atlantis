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
// Separate from AdoptBaseline, which both compares and records agreement.
// Asking only the comparison through `adopt --allow-drift` rewrites the shared
// checkpoint for every caller. This runs at CAPABILITY_SCHEMA_READ, so CI can
// ask it against production.
//
// The transaction is opened READ ONLY, so a write added to this path fails at
// Postgres. compareToLive is shared with adopt, where the following line is
// persistCheckpoint.
//
// No advisory lock, unlike adopt, which takes one because it rewrites the
// checkpoint and must serialise against apply. Taking it here would let a
// `tide inspect` in CI block a deploy.
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
