package admin

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
	"github.com/rachitkumar205/atlantis/internal/adopt"
	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// GenerateSchema writes .atl describing tables no declaration mentions.
//
// The pipeline is internal/adopt, which reads through a Querier and so runs
// against any database. This binds it to the server's own pool.
//
// Runs in the same READ ONLY transaction as InspectSchema, so the read-only
// guarantee is enforced by Postgres rather than by review.
func (s *Service) GenerateSchema(ctx context.Context, req *adminpb.GenerateSchemaRequest) (*adminpb.GenerateSchemaResponse, error) {
	ns := req.GetNamespace()
	if ns == "" {
		return nil, errors.New("admin: namespace is required — it becomes part of every " +
			"generated entity's ID, so the server will not choose one")
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// Introspection describes the managed database, which is the control one
	// unless this organisation adopted an existing database.
	live, releaseLive, err := s.liveTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	defer releaseLive()

	// Whatever is already declared, so its tables are not offered again.
	// Absent submissions this is nil, which adopt.Generate reads as "nothing
	// is declared yet" — the first run against a database atlantis has never
	// seen.
	var declaredIR *dsl.IR
	if subs := callerSubmissionsFromPB(req.GetSubmissions()); len(subs) > 0 {
		cmp, err := s.compareToLive(ctx, tx, live, subs)
		if err != nil {
			return nil, err
		}
		declaredIR = cmp.DeclaredIR
	}

	res, err := adopt.Generate(ctx, tx, ns, req.GetSchemas(), declaredIR)
	if err != nil {
		return nil, err
	}

	out := make([]*adminpb.GeneratedEntity, 0, len(res.Entities))
	for _, e := range res.Entities {
		out = append(out, &adminpb.GeneratedEntity{
			Table:      e.Table,
			EntityName: e.Name,
			Atl:        e.Atl,
		})
	}

	return &adminpb.GenerateSchemaResponse{
		Entities: out,
		Skipped:  res.Skipped,
		Warnings: res.Warnings,
	}, nil
}
