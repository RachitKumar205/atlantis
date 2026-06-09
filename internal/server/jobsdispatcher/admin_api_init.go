// Hook the default release-row implementation up to the SDK helper.
// Lives in its own file so admin_api.go's body can stay free of
// jobs-package imports (the cycle isn't real today but the separation
// keeps the dispatcher's observation API self-contained).

package jobsdispatcher

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rachitkumar205/atlantis/clients/go/jobs"
)

func defaultReleaseRow(ctx context.Context, pool *pgxpool.Pool, jobID int64, claimedBy, reason string) error {
	return jobs.ReleaseRow(ctx, pool, jobID, claimedBy, reason)
}
