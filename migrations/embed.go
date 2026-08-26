// Package migrations carries the SQL trees atlantis owns, embedded in the
// binaries that apply them.
//
// Embedded rather than read from a path, so a binary and the schema it was
// built against cannot be updated separately. A server started against an older
// checkout would migrate a database backwards from what its own code expects,
// and report success: applying no migrations and applying the right ones look
// identical from outside.
//
// With no path there is no mismatch, and no `COPY migrations` line in the
// server image (Dockerfile:150). The console image is a single binary.
//
// tidectl-emitted migrations are not here. `tidectl plan` writes those into the
// deployment's repository after this binary is built, so no binary can carry
// them; see internal/migrate.RunDir. atlantis owns infra/ and console/, and the
// deployment owns its own.
package migrations

import "embed"

// Infra is the server's own schema: the atlantis schema, its tables, the
// outbox, RLS helpers, and everything else the runtime depends on.
//
//go:embed infra/*.sql
var Infra embed.FS

// Console is the management console's schema: operators, sessions, and the
// partitioned audit log.
//
// Separate from Infra because the two are applied by different binaries against
// their own history tables. A console deployed without a server, or upgraded on
// its own, moves one and not the other.
//
//go:embed console/*.sql
var Console embed.FS

// Cloud is the control plane's schema: users, organisations, memberships, and
// the credentials people sign in with.
//
// A third tree for the same reason there is a second one — cmd/cloud is
// deployed, upgraded and rolled back on its own schedule, and it is the only
// thing that writes these tables. It also reaches its own database
// (CLOUD_PG_URL), which during development is the same PostgreSQL instance the
// console uses and need not stay that way: nothing here joins across the
// boundary, so separating them later is a connection string rather than a
// migration.
//
//go:embed cloud/*.sql
var Cloud embed.FS
