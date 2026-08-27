package adopt

import (
	"context"
	"fmt"
	"sort"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/introspect"
)

// GenerateAll reads every schema in a database, one namespace per schema.
//
// The whole-database entry point, for a caller that is adopting a database it
// has not catalogued. Generate takes a namespace and a schema list because the
// caller has decided both; here nothing has, so the database is asked what it
// holds and the schema name becomes the namespace.
//
// One namespace per schema rather than one for everything: two schemas may hold
// a table of the same name, and merged into one namespace the second would be
// reported as a collision and dropped.
//
// A schema is read or it appears in Skipped. Never both, and never neither.
func GenerateAll(ctx context.Context, q introspect.Querier, declaredIR *dsl.IR) (Result, error) {
	schemas, err := introspect.SchemasWithTables(ctx, q)
	if err != nil {
		return Result{}, err
	}
	if len(schemas) == 0 {
		return Result{}, fmt.Errorf("%w: that database has no tables", ErrNothingToRead)
	}

	var all Result
	taken := map[string]string{}
	for _, schema := range schemas {
		ns, note := namespaceFor(schema)
		if ns == "" {
			all.Skipped = append(all.Skipped, fmt.Sprintf(
				"schema %q: atlantis cannot address a schema by that name — it must "+
					"start with a letter or underscore and continue with letters, "+
					"digits or underscores. Its tables were not read", schema))
			continue
		}
		// Two schemas whose names differ only in characters a namespace cannot
		// carry would land in one namespace and collide table by table. The
		// first keeps the name and the second is reported.
		if prev, dup := taken[ns]; dup {
			all.Skipped = append(all.Skipped, fmt.Sprintf(
				"schema %q: its namespace name %q is already taken by schema %q — "+
					"read it on its own", schema, ns, prev))
			continue
		}
		taken[ns] = schema
		if note != "" {
			all.Warnings = append(all.Warnings, note)
		}

		res, err := Generate(ctx, q, ns, []string{schema}, declaredIR)
		if err != nil {
			return Result{}, fmt.Errorf("schema %q: %w", schema, err)
		}
		all.Entities = append(all.Entities, res.Entities...)
		all.Skipped = append(all.Skipped, res.Skipped...)
		all.Warnings = append(all.Warnings, res.Warnings...)
		all.Suggestions = append(all.Suggestions, res.Suggestions...)
	}

	sort.Slice(all.Entities, func(i, j int) bool { return all.Entities[i].Table < all.Entities[j].Table })
	sort.Strings(all.Skipped)
	sort.Strings(all.Warnings)
	sort.Slice(all.Suggestions, func(i, j int) bool {
		if all.Suggestions[i].Table != all.Suggestions[j].Table {
			return all.Suggestions[i].Table < all.Suggestions[j].Table
		}
		return all.Suggestions[i].Kind < all.Suggestions[j].Kind
	})
	return all, nil
}

// namespaceFor returns the namespace a schema is read into, or empty where it
// cannot be read at all.
//
// The schema name is used as it stands. A schema name is quoted in SQL and may
// hold anything, so `sales-eu` and `2024` are both legal; neither is a
// namespace, and neither can appear in a `table "..."` value either. Sanitising
// the namespace to `sales_eu` would produce a declaration that parses, names a
// table Lower rejects, and cannot be committed — so a schema atlantis cannot
// address is reported rather than approximated.
func namespaceFor(schema string) (ns, note string) {
	if !dsl.IsTableName(schema) {
		return "", ""
	}
	return schema, ""
}
