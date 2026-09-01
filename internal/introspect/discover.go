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

	// Partition children are excluded. They are ordinary relations in pg_class
	// and would otherwise be discovered as tables in their own right — one per
	// partition, none of them separately declarable, and on a time-partitioned
	// table there may be hundreds.
	//
	// Which test is used depends on a privilege. pg_inherits holds a row for a
	// child of either mechanism and is the complete answer; relispartition is
	// set only for declarative partitioning, so it admits every child of an
	// INHERITS parent — 112 of them from one public dataset.
	//
	// pg_inherits is readable by PUBLIC in a default installation and a
	// hardened database may revoke it. EBI's public mirror does, and reading it
	// unconditionally failed the whole import with `permission denied for table
	// pg_inherits`. pg_class carries relispartition and is always readable, so
	// the narrower test is the fallback rather than the failure.
	//
	// The atlantis schema holds this server's own machinery (jobs, the IR
	// checkpoint, caller registrations). Offering to generate declarations for
	// it would propose that a customer adopt our bookkeeping as their schema.
	//
	// The _timescaledb_* schemas hold chunk storage for hypertables. Same
	// reasoning as partition children, one layer down.
	const tablesSQL = `
SELECT n.nspname, c.relname
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r','p','f')
  AND __PARTITION_CHILD__
  AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'atlantis')
  AND n.nspname NOT LIKE 'pg\_toast%'
  AND n.nspname NOT LIKE 'pg\_temp%'
  AND n.nspname NOT LIKE '\_timescaledb%'
  AND (cardinality($1::text[]) = 0 OR n.nspname = ANY($1::text[]))
ORDER BY n.nspname, c.relname`
	query := strings.Replace(tablesSQL, "__PARTITION_CHILD__", partitionChildTest(ctx, q), 1)

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
	const schemasSQL = `
SELECT n.nspname
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r','p','f')
  AND __PARTITION_CHILD__
  AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'atlantis')
  AND n.nspname NOT LIKE 'pg\_toast%'
  AND n.nspname NOT LIKE 'pg\_temp%'
  AND n.nspname NOT LIKE '\_timescaledb%'
GROUP BY n.nspname
ORDER BY count(*) DESC, n.nspname`
	query := strings.Replace(schemasSQL, "__PARTITION_CHILD__", partitionChildTest(ctx, q), 1)

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

// partitionChildTest returns the SQL that excludes a partition child, using
// whichever catalogue the connection may actually read.
//
// pg_inherits covers both mechanisms and is the answer wherever it is
// readable. Where it is not, relispartition covers declarative partitioning
// only, and a child attached with INHERITS is discovered as a table of its
// own — which is what happened before pg_inherits was consulted at all.
//
// InheritsReadable reports which of the two applies, so a caller can say so.
func partitionChildTest(ctx context.Context, q Querier) string {
	if InheritsReadable(ctx, q) {
		return "NOT EXISTS (SELECT 1 FROM pg_inherits i WHERE i.inhrelid = c.oid)"
	}
	return "NOT c.relispartition"
}

// InheritsReadable reports whether the connection may read pg_inherits.
//
// Readable by PUBLIC in a default installation; a hardened database may revoke
// it, and reading it anyway fails the statement outright rather than returning
// fewer rows. Asked with has_table_privilege, which answers without touching
// the table.
func InheritsReadable(ctx context.Context, q Querier) bool {
	rows, err := q.Query(ctx,
		`SELECT has_table_privilege('pg_catalog.pg_inherits', 'SELECT')`)
	if err != nil {
		return false
	}
	defer rows.Close()
	var ok bool
	if rows.Next() {
		if err := rows.Scan(&ok); err != nil {
			return false
		}
	}
	return ok && rows.Err() == nil
}
