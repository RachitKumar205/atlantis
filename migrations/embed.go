// Package migrations carries the SQL trees atlantis owns, embedded in the
// binaries that apply them.
//
// # Why embedded rather than read from disk
//
// A binary and the schema it was built against are one artifact. Reading the
// tree from a path meant they could be updated separately: a server started
// with MIGRATIONS_DIR pointing at an older checkout would migrate a database
// backwards from what its own code expects, and nothing would say so — the run
// reports success either way, because applying no migrations and applying the
// right ones look identical from outside.
//
// Embedding removes the path, so it removes the mismatch. It also removes the
// `COPY migrations /app/migrations` line the server image needed, and lets the
// console image stay a single binary.
//
// # What is NOT here
//
// tidectl-emitted migrations. Those are written by `tidectl plan` / `approve`
// into the DEPLOYMENT's repository, after this binary is built, so no binary
// can carry them — see internal/migrate.RunDir. The split is by who owns the
// schema: atlantis owns infra/ and console/, the deployment owns its own.
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
