// Package adopt turns a live database into .atl describing it.
//
//	discover tables → stub entity per table → introspect → emit .atl
//
// The pipeline reads through an introspect.Querier, so it runs against any
// database the caller can open: the admin RPC passes a transaction on the
// server's own pool, and onboarding passes one on a database supplied by
// whoever is adopting it.
//
// Everything here reads. The caller supplies the transaction and is what makes
// it READ ONLY.
package adopt

import (
	"context"
	"fmt"
	"sort"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/dsl/atlemit"
	"github.com/rachitkumar205/atlantis/internal/introspect"
)

// Entity is one generated declaration.
type Entity struct {
	// Table is the schema-qualified physical table it describes.
	Table string

	// Name is the proposed entity name. A proposal: it becomes part of the
	// entity ID every caller references, so renaming it after adoption is a
	// breaking change.
	Name string

	// Atl is the declaration, ready to review and commit.
	Atl string
}

// Result is what one pass over a database produced.
type Result struct {
	Entities []Entity

	// Skipped names a discovered table and why no declaration was written for
	// it. A table appears here or in Entities, never both.
	Skipped []string

	// Warnings are facts introspection did not verify, plus security
	// heuristics. Advisory: they describe the database, not this run.
	Warnings []string
}

// Generate writes .atl describing tables declaredIR does not mention.
//
// declaredIR may be nil, which DiscoverTables reads as "nothing is declared
// yet" — the first run against a database atlantis has never seen.
//
// ns becomes part of every generated entity's ID, so it is required and no
// default is chosen here.
func Generate(ctx context.Context, q introspect.Querier, ns string, schemas []string, declaredIR *dsl.IR) (Result, error) {
	if ns == "" {
		return Result{}, fmt.Errorf("adopt: namespace is required — it becomes part of " +
			"every generated entity's ID")
	}

	found, err := introspect.DiscoverTables(ctx, q, declaredIR, schemas)
	if err != nil {
		return Result{}, err
	}
	if len(found) == 0 {
		return Result{}, nil
	}

	stubs := &dsl.IR{Entities: make([]dsl.Entity, 0, len(found))}
	nameFor := make(map[string]string, len(found))
	var skipped []string

	// Seeded from what is ALREADY declared, not only from what this run
	// discovers. DiscoverTables excludes declared TABLES, but two different
	// tables can suggest the same entity NAME — a declared
	// `entity Order in shop { table "legacy.orders" }` alongside an
	// undiscovered `legacy.order` both want to be shop.Order.
	//
	// Without this the response offered a second Order with skipped empty, and
	// committing it made dsl.Lower fail on a duplicate entity ID, which breaks
	// every later plan, apply and inspect for that caller. The collision was
	// already handled between two discovered tables; only the declared side
	// was missing.
	seen := map[string]bool{}
	declaredNames := map[string]bool{}
	if declaredIR != nil {
		for i := range declaredIR.Entities {
			if declaredIR.Entities[i].Namespace == ns {
				seen[declaredIR.Entities[i].Name] = true
				declaredNames[declaredIR.Entities[i].Name] = true
			}
		}
	}

	for _, d := range found {
		name := d.SuggestedName()
		if name == "" {
			skipped = append(skipped, fmt.Sprintf("%s: no entity name could be derived from the table name", d.Qualified()))
			continue
		}
		// SuggestedName only treats `_`, `-` and ` ` as separators and copies
		// every other rune through, so a table named "a/b" yields "A/b" and one
		// named "2024_events" yields "2024Events". Neither lexes as an
		// identifier, so the .atl this would emit does not parse.
		//
		// Refused here rather than left to the client. `tide inspect
		// --generate` does check before writing a file, but it is one consumer
		// of this pipeline and the console is another; a server that hands back
		// an entity it knows cannot parse is proposing work that cannot
		// succeed.
		if !dsl.IsIdentifier(name) {
			skipped = append(skipped, fmt.Sprintf(
				"%s: %q is not a usable entity name — an .atl identifier starts with "+
					"a letter or underscore and continues with letters, digits or "+
					"underscores. Rename the table or declare it by hand",
				d.Qualified(), name))
			continue
		}
		// Two tables in different Postgres schemas can share a name, and both
		// would land in the one atlantis namespace this request names. The
		// second is reported rather than silently overwriting the first.
		//
		// The two causes get different messages because the remedy differs: a
		// clash with an existing declaration is resolved by renaming one of
		// them, a clash between two discovered tables by generating into
		// separate namespaces.
		if declaredNames[name] {
			skipped = append(skipped, fmt.Sprintf(
				"%s: entity name %q is already declared in namespace %q — rename "+
					"the entity or declare this table by hand", d.Qualified(), name, ns))
			continue
		}
		if seen[name] {
			skipped = append(skipped, fmt.Sprintf(
				"%s: entity name %q is already taken by another discovered table — "+
					"generate it separately into its own namespace", d.Qualified(), name))
			continue
		}
		seen[name] = true
		nameFor[ns+"."+name] = d.Qualified()
		stubs.Entities = append(stubs.Entities, dsl.Entity{
			Name: name, Namespace: ns, TableName: d.Qualified(),
		})
	}
	if len(stubs.Entities) == 0 {
		return Result{Skipped: skipped}, nil
	}

	filled, _, warnings, err := introspect.FromPostgres(ctx, q, stubs)
	if err != nil {
		return Result{}, fmt.Errorf("introspect discovered tables: %w", err)
	}

	// FromPostgres carries CHECKs and composite UNIQUEs from the declaration it
	// was given, which here is a stub holding neither. Reading them is what
	// makes the emitted .atl describe the database instead of the stub.
	if err := introspect.Enrich(ctx, q, filled); err != nil {
		return Result{}, err
	}

	out := make([]Entity, 0, len(filled.Entities))
	for i := range filled.Entities {
		e := &filled.Entities[i]
		table := nameFor[e.ID()]
		out = append(out, Entity{
			Table: table,
			Name:  e.Name,
			Atl:   atlemit.Entity(e, table),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Table < out[j].Table })

	return Result{Entities: out, Skipped: skipped, Warnings: warnings}, nil
}
