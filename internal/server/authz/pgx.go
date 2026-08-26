package authz

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool adapts a pgxpool to the narrow Querier this package needs.
//
// pgx returns a concrete pgx.Rows while Querier returns the local Rows
// interface, and Go requires exact return types for method-set matching.
// Keeping Querier local rather than depending on pgx directly is what lets the
// grant tests run without Postgres.
func Pool(p *pgxpool.Pool) Querier { return pgxQuerier{p} }

type pgxQuerier struct{ p *pgxpool.Pool }

func (q pgxQuerier) Query(ctx context.Context, sql string, args ...any) (Rows, error) {
	rows, err := q.p.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgxRows{rows}, nil
}

type pgxRows struct{ pgx.Rows }

func (r pgxRows) Close() { r.Rows.Close() }
