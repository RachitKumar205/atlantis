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
	"errors"
	"fmt"
	"sort"
	"strings"

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

	// Namespace is what the declaration says `in`. GenerateAll sets one per
	// Postgres schema, so a result covering several schemas carries several.
	Namespace string

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

	// Suggestions are changes worth making to the tables described above. Each
	// carries the .atl line that would make it.
	Suggestions []Suggestion
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
		return Result{}, noTablesError(ctx, q, schemas)
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
		// The physical name, before the entity name. `table "..."` is copied
		// into the generated .atl verbatim and Lower matches it against
		// [schema.]table, so a table in a schema named `sales-eu` produces a
		// declaration that parses and will not lower — which is a file that
		// cannot be committed, handed back as though it could.
		//
		// Checked here rather than at the emitter because the remedy is to
		// leave the table out and say so, and only this loop can do that.
		if !dsl.IsTableName(d.Qualified()) {
			skipped = append(skipped, fmt.Sprintf(
				"%s: atlantis cannot address that table — a `table \"...\"` value is "+
					"[schema.]table where each part starts with a letter or underscore "+
					"and continues with letters, digits or underscores", d.Qualified()))
			continue
		}
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
	notes, err := introspect.Enrich(ctx, q, filled)
	if err != nil {
		return Result{}, err
	}
	warnings = append(warnings, notes...)
	sort.Strings(warnings)

	// Before the drop, so a table left out still carries the advice that would
	// let it be read next time — "add a primary key" is the remedy for the
	// commonest reason one is left out.
	suggestions := Suggest(filled, nameFor)

	skipped = append(skipped, dropUndeclarableEntities(filled, nameFor)...)
	sort.Strings(skipped)

	out := make([]Entity, 0, len(filled.Entities))
	for i := range filled.Entities {
		e := &filled.Entities[i]
		table := nameFor[e.ID()]
		out = append(out, Entity{
			Table:     table,
			Name:      e.Name,
			Namespace: e.Namespace,
			Atl:       atlemit.Entity(e, table),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Table < out[j].Table })

	return Result{
		Entities:    out,
		Skipped:     skipped,
		Warnings:    warnings,
		Suggestions: suggestions,
	}, nil
}

// ErrNothingToRead reports a request that named no readable tables.
//
// A property of the request, not of the database or the connection to it: the
// remedy is to name a different schema. Callers that answer over HTTP use this
// to say so with a 400 rather than reporting a failure to reach the database.
var ErrNothingToRead = errors.New("no tables to read")

// noTablesError explains a discovery that found nothing, or returns nil where
// the empty result is the right answer.
//
// Two databases produce the same empty list and want different things said. A
// schema that holds no tables was probably the wrong schema to ask for — the
// remedy is to name another, so the message names the ones that would work. A
// schema whose tables are all declared already is a finished adoption, and
// reporting that as a failure would make a second run of `tide inspect
// --generate` look broken.
//
// Postgres defaults `search_path` to "public", so "public" is what a caller
// supplies without thinking about it, and a database keeping its tables
// anywhere else answers "0 tables" to a request that was never going to match.
func noTablesError(ctx context.Context, q introspect.Querier, schemas []string) error {
	available, err := introspect.SchemasWithTables(ctx, q)
	if err != nil {
		return err
	}
	if len(available) == 0 {
		return fmt.Errorf("%w: that database has no tables", ErrNothingToRead)
	}

	// Tables exist somewhere. If they exist in a schema that was asked for,
	// every one of them is already declared.
	asked := map[string]bool{}
	for _, s := range schemas {
		asked[s] = true
	}
	if len(asked) == 0 {
		return nil
	}
	for _, s := range available {
		if asked[s] {
			return nil
		}
	}

	const show = 8
	names := available
	suffix := ""
	if len(names) > show {
		names, suffix = names[:show], fmt.Sprintf(" and %d more", len(available)-show)
	}
	return fmt.Errorf("%w: nothing in %s. This database keeps its tables in %s%s — "+
		"name the schema you want to read",
		ErrNothingToRead, strings.Join(schemas, ", "), strings.Join(names, ", "), suffix)
}

// dropUndeclarableEntities removes tables whose declaration would not parse,
// and reports each one.
//
// A column name is a field name in .atl, and .atl reserves words the catalogue
// does not. A table with a column called `identity` — a percent-identity
// measure on a public bioinformatics dataset — emits `identity double`, which
// the parser reads as a modifier and then fails on the following field. One
// such column makes the whole FILE unparseable, so it takes every table in the
// namespace with it.
//
// The table goes rather than the column. A declaration missing a column is one
// the next plan compares against the real table and proposes reconciling, and
// the column it would be reconciling away holds data.
func dropUndeclarableEntities(ir *dsl.IR, nameFor map[string]string) []string {
	var skipped []string
	kept := ir.Entities[:0]
	for i := range ir.Entities {
		e := &ir.Entities[i]
		bad := ""
		for j := range e.Fields {
			if !dsl.CanBeFieldName(e.Fields[j].Name) {
				bad = e.Fields[j].Name
				break
			}
		}
		switch {
		case bad != "":
			skipped = append(skipped, fmt.Sprintf(
				"%s: column %q is a reserved word in .atl and cannot be a field name, "+
					"so no declaration was written for this table — rename the column, "+
					"or declare the table by hand with the rest of its columns",
				nameFor[e.ID()], bad))

		// Every entity needs one, and Lower says so — so a table without one
		// produces a declaration that parses and takes the whole file down with
		// it at `tide plan`. hasPrimaryKey rather than a second predicate: the
		// no-primary-key suggestion is raised by the same function, and the
		// table must not be both suggested and emitted.
		default:
			// A table with no key is declared `keyless`: atlantis owns its
			// schema and serves no API for it, because with no key there is no
			// Get, no Update or Delete by key, and the keyset cursor has no
			// non-nullable tiebreaker to advance on. adopt.Suggest names the
			// key that would give it one.
			if !hasPrimaryKey(e) {
				e.Keyless = true
				// `primary by` naming fields no column resolves to leaves the
				// clause behind, and `keyless` with a key beside it is a pair
				// validateEntity refuses. Suggest has already run, above, so
				// clearing this changes no suggestion.
				e.CompositePK = nil
			}
			kept = append(kept, *e)
		}
	}
	ir.Entities = kept

	// A dropped table takes every foreign key that pointed at it.
	//
	// Lower resolves `references` against the entities in the file, so a
	// declaration naming one that was left out fails — and it fails for the
	// whole namespace, which is how one PK-less table stopped 61 good ones from
	// being usable. The column stays and loses its reference; the note says the
	// constraint is not declared, because it is the reference and not the
	// column that cannot be spelled.
	live := make(map[string]bool, len(kept))
	for i := range kept {
		live[kept[i].ID()] = true
	}
	for i := range ir.Entities {
		e := &ir.Entities[i]
		for j := range e.Fields {
			f := &e.Fields[j]
			if f.Ref == nil || live[f.Ref.TargetID] {
				continue
			}
			skipped = append(skipped, fmt.Sprintf(
				"%s: column %q references %s, which was not declared, so the foreign "+
					"key is left out and a plan will propose dropping it",
				nameFor[e.ID()], f.Name, f.Ref.TargetID))
			f.Ref = nil
		}
	}
	return skipped
}
