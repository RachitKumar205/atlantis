package introspect

import (
	"context"
	"fmt"
	"strings"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// DiscoveredTable is a physical table no declaration mentions.
//
// Deliberately not a dsl.Entity. Naming it — which namespace, what entity
// name — is a decision with consequences a discovery pass has no standing to
// make: those names become the caller's generated API surface, and renaming
// one later is a breaking change that goes through the approval gate. This
// type carries what the catalogue says and stops there.
type DiscoveredTable struct {
	Schema string
	Table  string
}

// Qualified renders the schema-qualified name, which is what a generated
// entity's `table` clause has to carry.
func (d DiscoveredTable) Qualified() string { return d.Schema + "." + d.Table }

// SuggestedName proposes an entity name: user_accounts becomes UserAccounts.
//
// A proposal, and named so. This string becomes a generated Go type, a proto
// message and part of the entity ID every other caller references, so renaming
// it after adoption is a breaking change that goes through the approval gate.
// Whatever calls this must offer the result for editing rather than applying
// it silently.
//
// Deliberately does NOT singularise. "users" → "User" reads better and is
// right most of the time, but English plurals are irregular enough that it is
// wrong often enough to matter — and a wrong name that looks deliberate is
// harder to notice than a clumsy one. Doubling the table name is a choice the
// customer can see and change.
func (d DiscoveredTable) SuggestedName() string {
	var b strings.Builder
	b.Grow(len(d.Table))
	upper := true
	for _, r := range d.Table {
		switch {
		case r == '_' || r == '-' || r == ' ':
			upper = true
		case upper:
			b.WriteString(strings.ToUpper(string(r)))
			upper = false
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// atlantisOwnedInPublic are tables atlantis creates in the public schema for
// its own bookkeeping.
//
// Excluded by EXACT name rather than an `atlantis_%` prefix. A prefix would be
// shorter and would also silently swallow a customer table that happened to
// start with it — and dropping one of their tables from a discovery listing is
// a worse failure than showing them one of ours, because the first is
// invisible and the second is one deselection.
//
// Sourced from internal/migrate/migrate.go, which passes these as
// x-migrations-table with search_path=public.
var atlantisOwnedInPublic = map[string]bool{
	"atlantis_schema_migrations_infra":   true,
	"atlantis_schema_migrations_tidectl": true,
}

// DiscoverTables lists tables that physically exist and that declaredIR does
// not describe.
//
// FromPostgres verifies only what it is told about: loadExistingTables filters
// pg_class down to the declared pairs, so adoption requires a declaration for
// every table already written. A legacy database being adopted is precisely the
// case where those files do not exist yet.
//
// Pass nil schemas to search every non-system schema, which is what onboarding
// wants. Pass an explicit list to narrow it.
//
// declaredIR may be nil, meaning "nothing is declared yet" — the first run
// against a database atlantis has never seen.
func DiscoverTables(ctx context.Context, q Querier, declaredIR *dsl.IR, schemas []string) ([]DiscoveredTable, error) {
	known := map[physRef]bool{}
	if declaredIR != nil {
		for i := range declaredIR.Entities {
			s, t := physical(&declaredIR.Entities[i])
			known[physRef{schema: s, table: t}] = true
		}
	}

	// pg_inherits excludes the children of a partitioned table, whichever
	// mechanism attaches them. They are ordinary relations in pg_class and
	// would otherwise be discovered as tables in their own right — one per
	// partition, none of them separately declarable, and on a time-partitioned
	// table there may be hundreds.
	//
	// Not relispartition, which Postgres sets only for declarative
	// partitioning. A child attached with INHERITS has it false and a
	// pg_inherits row, so the narrower test admits every child of an
	// inheritance parent: 112 arrived as skipped tables from one public
	// dataset. A declarative partition has a pg_inherits row too, so this
	// covers both.
	//
	// The atlantis schema holds this server's own machinery (jobs, the IR
	// checkpoint, caller registrations). Offering to generate declarations for
	// it would propose that a customer adopt our bookkeeping as their schema.
	//
	// The _timescaledb_* schemas hold chunk storage for hypertables. Same
	// reasoning as partition children, one layer down.
	const query = `
SELECT n.nspname, c.relname
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r','p','f')
  AND NOT EXISTS (SELECT 1 FROM pg_inherits i WHERE i.inhrelid = c.oid)
  AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'atlantis')
  AND n.nspname NOT LIKE 'pg\_toast%'
  AND n.nspname NOT LIKE 'pg\_temp%'
  AND n.nspname NOT LIKE '\_timescaledb%'
  AND (cardinality($1::text[]) = 0 OR n.nspname = ANY($1::text[]))
ORDER BY n.nspname, c.relname`

	if schemas == nil {
		schemas = []string{}
	}
	rows, err := q.Query(ctx, query, schemas)
	if err != nil {
		return nil, fmt.Errorf("discover tables: %w", err)
	}
	defer rows.Close()

	var out []DiscoveredTable
	for rows.Next() {
		var d DiscoveredTable
		if err := rows.Scan(&d.Schema, &d.Table); err != nil {
			return nil, fmt.Errorf("scan discovered table: %w", err)
		}
		if known[physRef{schema: d.Schema, table: d.Table}] {
			continue
		}
		if d.Schema == "public" && atlantisOwnedInPublic[d.Table] {
			continue
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// SchemasWithTables names every schema DiscoverTables would look in that holds
// at least one table.
//
// What a read of the requested schemas cannot say on its own: finding nothing
// there is indistinguishable from a database with nothing in it, and the two
// have different remedies. The exclusions match DiscoverTables, so a schema
// named here is one that would yield tables if it were asked for.
func SchemasWithTables(ctx context.Context, q Querier) ([]string, error) {
	const query = `
SELECT n.nspname
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r','p','f')
  AND NOT EXISTS (SELECT 1 FROM pg_inherits i WHERE i.inhrelid = c.oid)
  AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'atlantis')
  AND n.nspname NOT LIKE 'pg\_toast%'
  AND n.nspname NOT LIKE 'pg\_temp%'
  AND n.nspname NOT LIKE '\_timescaledb%'
GROUP BY n.nspname
ORDER BY count(*) DESC, n.nspname`

	rows, err := q.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list schemas holding tables: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("scan schema: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
