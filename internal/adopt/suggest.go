package adopt

import (
	"fmt"
	"sort"
	"strings"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/schema"
)

// Suggestion is one change worth making to a discovered table.
//
// Advice, never applied. Each names the .atl line that would make it, so
// acting on one is a paste rather than a translation.
type Suggestion struct {
	// Entity is the generated entity's ID, and Table the physical table it
	// describes.
	Entity string
	Table  string

	// Kind groups suggestions of the same shape.
	Kind string

	// Detail says what was found and what it costs.
	Detail string

	// Line is the .atl to add, empty where the remedy is not one line.
	Line string
}

// Suggestion kinds.
const (
	// SuggestTenantIsolation: a tenant-shaped column with no row-level
	// security behind it.
	SuggestTenantIsolation = "tenant-isolation"

	// SuggestNoPrimaryKey: a table with no primary key.
	SuggestNoPrimaryKey = "no-primary-key"

	// SuggestUnindexedForeignKey: a foreign key with no index leading on its
	// column.
	SuggestUnindexedForeignKey = "unindexed-foreign-key"
)

// tenantColumns are the column names taken to mean "which customer owns this
// row".
//
// A fixed list, and a short one. Every name here is one whose whole purpose is
// tenancy; `customer_id` and `user_id` are absent because on most schemas they
// are ordinary foreign keys, and a suggestion that fires on every table teaches
// the reader to skip all of them.
var tenantColumns = map[string]bool{
	"tenant_id":       true,
	"org_id":          true,
	"organisation_id": true,
	"organization_id": true,
	"workspace_id":    true,
	"account_id":      true,
}

// Suggest reads the enriched IR and proposes what would tighten it.
//
// Pure: everything it needs was read during introspection. PartitionField
// carries whatever pg_policy holds, so a table already isolated is one this
// stays quiet about.
//
// physicalByID maps entity ID to the table it was generated from, so a
// suggestion names the table the reader recognises.
func Suggest(ir *dsl.IR, physicalByID map[string]string) []Suggestion {
	if ir == nil {
		return nil
	}
	var out []Suggestion

	for i := range ir.Entities {
		e := &ir.Entities[i]
		table := physicalByID[e.ID()]

		out = append(out, tenantSuggestions(e, table)...)
		if !hasPrimaryKey(e) {
			out = append(out, Suggestion{
				Entity: e.ID(), Table: table, Kind: SuggestNoPrimaryKey,
				Detail: "no primary key, so a row cannot be addressed and atlantis " +
					"generates no Get, Update or Delete for this entity",
			})
		}
		out = append(out, unindexedForeignKeys(e, table)...)
	}

	sort.SliceStable(out, func(a, b int) bool {
		if out[a].Table != out[b].Table {
			return out[a].Table < out[b].Table
		}
		return out[a].Kind < out[b].Kind
	})
	return out
}

// tenantSuggestions proposes `partition by` where a tenant column carries no
// policy.
//
// PartitionField non-empty means Postgres already has ENABLE + FORCE ROW LEVEL
// SECURITY and a policy on that column, which is the thing being suggested.
func tenantSuggestions(e *dsl.Entity, table string) []Suggestion {
	if e.PartitionField != "" {
		return nil
	}
	var out []Suggestion
	for i := range e.Fields {
		f := &e.Fields[i]
		if !tenantColumns[strings.ToLower(f.Name)] {
			continue
		}

		detail := fmt.Sprintf(
			"%s looks like a tenant key and no row-level security references it, "+
				"so isolation rests on every query remembering to filter. "+
				"`partition by` emits ENABLE + FORCE ROW LEVEL SECURITY and a policy "+
				"comparing the column to atlantis.current_partition(), which Postgres "+
				"enforces on reads this server did not write", f.Name)

		// A NULL never equals current_partition(), so under FORCE ROW LEVEL
		// SECURITY a row with no tenant is a row nothing reaches — including
		// the process that has to fix it.
		if !f.NotNull {
			detail += ". " + f.Name + " is nullable: rows holding NULL match no " +
				"policy and become unreadable, so make it NOT NULL first"
		}

		out = append(out, Suggestion{
			Entity: e.ID(), Table: table, Kind: SuggestTenantIsolation,
			Detail: detail,
			Line:   "partition by " + f.Name,
		})
	}
	return out
}

// hasPrimaryKey reports whether the entity has one, single or composite.
//
// schema.PKColumns is the function the server resolves an entity's key columns
// with, and it drops each name in `primary by` that no field answers to.
// Counting the clause alone reports a key where the server resolves no column,
// and this suggestion is the only warning a table with no addressable row gets.
func hasPrimaryKey(e *dsl.Entity) bool {
	return len(schema.PKColumns(e)) > 0
}

// unindexedForeignKeys proposes an index for each foreign key with none.
//
// Postgres indexes the parent side of a foreign key and not the child. Without
// one, every DELETE or key UPDATE on the parent scans the whole child table to
// check the constraint, and the lock it takes is held for that scan.
//
// Leading column only. An index on (b, a) does not serve a lookup by a, so
// counting it would report the problem solved while it is not.
func unindexedForeignKeys(e *dsl.Entity, table string) []Suggestion {
	leading := map[string]bool{}
	for _, ix := range e.Indexes {
		if len(ix.Fields) > 0 && ix.Fields[0].Name != "" {
			leading[ix.Fields[0].Name] = true
		}
	}
	// The primary key's own index serves a lookup on its leading column.
	if len(e.CompositePK) > 0 {
		leading[e.CompositePK[0]] = true
	}
	for i := range e.Fields {
		f := &e.Fields[i]
		if f.Primary || f.Unique {
			leading[f.Name] = true
		}
	}
	for _, u := range e.Uniques {
		if len(u.Fields) > 0 {
			leading[u.Fields[0]] = true
		}
	}

	var out []Suggestion
	for i := range e.Fields {
		f := &e.Fields[i]
		if f.Ref == nil || leading[f.Name] {
			continue
		}
		out = append(out, Suggestion{
			Entity: e.ID(), Table: table, Kind: SuggestUnindexedForeignKey,
			Detail: fmt.Sprintf(
				"%s is a foreign key with no index leading on it, so every delete or "+
					"key update on the referenced table scans this one to check the "+
					"constraint, holding its lock for that scan", f.Name),
			Line: "index by " + f.Name,
		})
	}
	return out
}
