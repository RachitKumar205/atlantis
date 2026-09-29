package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/rachitkumar205/atlantis/internal/codegen"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/introspect"
	"github.com/rachitkumar205/atlantis/internal/schema"
)

// parkedTwin is a parked column that sits where codegen's
// emitFieldRestoreIfParked looks for one to restore an added field.
type parkedTwin struct {
	key             string // "entityID.fieldName"
	entityID, field string
	live            string // format_type of the parked column
	liveOID         uint32
	liveMod         int32
	parked          *dsl.Field // the declaration schema history records; nil when it has none
	declared        string     // parked's type as the migration spells it
	sameType        bool
}

// diffWithParked is codegen.ComputeDiff with the parked columns in db taken
// into account. An added field whose parked column the migration renames back
// is restored as schema history declares it, then diffed against its new
// declaration; see codegen.WithParkedFields.
//
// It also returns one message per added field whose parked column cannot be
// restored that way. A plan with any cannot apply.
//
// db must be the database the migration runs against, and history the one
// that holds atlantis.schema_versions. A diff that adds no field to an
// existing entity costs no query, and one that does costs one. A parked
// column adds one read of history and up to two type queries.
func diffWithParked(ctx context.Context, db, history introspect.Querier, prior, newIR *dsl.IR, opts ...codegen.DiffOption) (*codegen.Diff, []string, error) {
	d := codegen.ComputeDiff(prior, newIR, opts...)
	twins, err := findParkedTwins(ctx, db, newIR, d)
	if err != nil {
		return nil, nil, fmt.Errorf("read parked columns: %w", err)
	}
	if len(twins) == 0 {
		return d, nil, nil
	}
	if err := readParkedDeclarations(ctx, history, twins); err != nil {
		return nil, nil, fmt.Errorf("read parked declarations: %w", err)
	}
	if err := compareTypes(ctx, db, twins); err != nil {
		return nil, nil, fmt.Errorf("compare parked types: %w", err)
	}

	parked := map[string]*dsl.Field{}
	var conflicts []string
	for _, t := range twins {
		switch {
		case t.parked == nil:
			conflicts = append(conflicts, fmt.Sprintf("%s: a parked column holds this "+
				"field's rows, and schema history has no declaration for it, so the plan "+
				"cannot tell what the restore changes. Give the new field another name", t.key))
		case !t.sameType:
			conflicts = append(conflicts, fmt.Sprintf("%s: its parked column is %s, and "+
				"schema history records it as %s. Change the parked column back to that "+
				"type, or give the new field another name", t.key, t.live, t.declared))
		default:
			parked[t.key] = t.parked
		}
	}
	if len(parked) > 0 {
		opts = append(opts[:len(opts):len(opts)], codegen.WithParkedFields(parked))
		d = codegen.ComputeDiff(prior, newIR, opts...)
	}
	return d, conflicts, nil
}

// findParkedTwins returns the parked twins of the fields d adds, sorted by
// key.
func findParkedTwins(ctx context.Context, q introspect.Querier, newIR *dsl.IR, d *codegen.Diff) ([]parkedTwin, error) {
	var byID map[string]*dsl.Entity
	type added struct{ entityID, field string }
	byKey := map[string]added{}
	var keys, schemas, tables, columns []string
	for _, ch := range d.All() {
		if ch.Kind != codegen.KindFieldAdded {
			continue
		}
		if byID == nil {
			byID = make(map[string]*dsl.Entity, len(newIR.Entities))
			for i := range newIR.Entities {
				byID[newIR.Entities[i].ID()] = &newIR.Entities[i]
			}
		}
		e := byID[ch.EntityID]
		sch, tbl, col := codegen.ParkedColumn(e, ch.Field)
		key := ch.EntityID + "." + ch.Field
		byKey[key] = added{ch.EntityID, ch.Field}
		keys = append(keys, key)
		schemas = append(schemas, sch)
		tables = append(tables, tbl)
		columns = append(columns, col)
	}
	if len(keys) == 0 {
		return nil, nil
	}

	// The predicate emitFieldRestoreIfParked's DO block tests, one row per
	// parked column found.
	rows, err := q.Query(ctx, `
SELECT t.key, a.atttypid, a.atttypmod, format_type(a.atttypid, a.atttypmod)
  FROM unnest($1::text[], $2::text[], $3::text[], $4::text[]) AS t(key, sch, tbl, col)
  JOIN pg_attribute a
    ON a.attrelid = to_regclass(format('%I.%I', t.sch, t.tbl))
   AND a.attname = t.col AND a.attnum > 0 AND NOT a.attisdropped`,
		keys, schemas, tables, columns)
	if err != nil {
		return nil, err
	}
	var twins []parkedTwin
	for rows.Next() {
		var t parkedTwin
		if err := rows.Scan(&t.key, &t.liveOID, &t.liveMod, &t.live); err != nil {
			rows.Close()
			return nil, err
		}
		t.entityID, t.field = byKey[t.key].entityID, byKey[t.key].field
		twins = append(twins, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(twins, func(i, j int) bool { return twins[i].key < twins[j].key })
	return twins, nil
}

// readParkedDeclarations sets parked and declared on each twin from the most
// recent schema version that removed its field.
func readParkedDeclarations(ctx context.Context, q introspect.Querier, twins []parkedTwin) error {
	keys := make([]string, len(twins))
	ids := make([]string, len(twins))
	fields := make([]string, len(twins))
	at := make(map[string]int, len(twins))
	for i, t := range twins {
		keys[i], ids[i], fields[i] = t.key, t.entityID, t.field
		at[t.key] = i
	}
	// Newest first with LIMIT 1, so a backward walk of the primary key can
	// stop at the latest removal.
	//
	// A removal's Change carries the removed field's declaration as From,
	// whichever class bucket it landed in.
	rows, err := q.Query(ctx, `
SELECT t.key, r.decl
  FROM unnest($1::text[], $2::text[], $3::text[]) AS t(key, entity_id, field)
 CROSS JOIN LATERAL (
       SELECT c->'from' AS decl
         FROM atlantis.schema_versions v
        CROSS JOIN LATERAL jsonb_path_query(v.diff, '$.*[*] ? (@.kind == "field_removed")') c
        WHERE c->>'entity_id' = t.entity_id AND c->>'field' = t.field
        ORDER BY v.version DESC
        LIMIT 1) r`, keys, ids, fields)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var raw []byte
		if err := rows.Scan(&key, &raw); err != nil {
			return err
		}
		if raw == nil {
			continue
		}
		var f dsl.Field
		if err := json.Unmarshal(raw, &f); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		i := at[key]
		twins[i].parked = &f
		twins[i].declared = schema.SQLType(f.Type)
	}
	return rows.Err()
}

// compareTypes sets sameType on each twin with a declaration.
func compareTypes(ctx context.Context, q introspect.Querier, twins []parkedTwin) error {
	var idx []int
	var live []liveType
	var decls []string
	for i := range twins {
		if twins[i].parked != nil {
			idx = append(idx, i)
			live = append(live, liveType{twins[i].liveOID, twins[i].liveMod})
			decls = append(decls, twins[i].declared)
		}
	}
	same, err := sameTypes(ctx, q, live, decls)
	if err != nil {
		return err
	}
	for k, i := range idx {
		twins[i].sameType = same[k]
	}
	return nil
}

// liveType is a column's type as pg_attribute records it.
type liveType struct {
	oid uint32
	mod int32
}

// sameTypes reports for each i whether live[i] is the type declared[i] names,
// typmod included. A declared type that does not resolve is not the same.
// It costs one query, and a second when a base type matches.
func sameTypes(ctx context.Context, q introspect.Querier, live []liveType, declared []string) ([]bool, error) {
	same := make([]bool, len(live))
	if len(live) == 0 {
		return same, nil
	}
	rows, err := q.Query(ctx, `
SELECT coalesce(to_regtype(t.declared)::oid, 0)
  FROM unnest($1::text[]) WITH ORDINALITY AS t(declared, i)
 ORDER BY t.i`, declared)
	if err != nil {
		return nil, err
	}
	var idx []int
	var exprs []string
	k := 0
	for rows.Next() {
		var base uint32
		if err := rows.Scan(&base); err != nil {
			rows.Close()
			return nil, err
		}
		if base == live[k].oid {
			idx = append(idx, k)
			exprs = append(exprs, "NULL::"+declared[k])
		}
		k++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(idx) == 0 {
		return same, nil
	}

	// The result description of a SELECT of typed NULLs carries each type's
	// typmod: `NULL::VARCHAR(255)` is varchar with typmod 259, the atttypmod
	// of a varchar(255) column. Only resolved types are selected, because one
	// that does not exist fails the whole statement.
	rows, err = q.Query(ctx, "SELECT "+strings.Join(exprs, ", ")+" LIMIT 0")
	if err != nil {
		return nil, err
	}
	// Compared before Close: the descriptions live in the connection's
	// buffer, and Close returns a pooled connection to other queries.
	fds := rows.FieldDescriptions()
	described := len(fds)
	if described == len(idx) {
		for n, i := range idx {
			same[i] = fds[n].DataTypeOID == live[i].oid && fds[n].TypeModifier == live[i].mod
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if described != len(idx) {
		return nil, fmt.Errorf("described %d declared types, want %d", described, len(idx))
	}
	return same, nil
}

// parkedColumn is a registered, unreaped parked column that still exists.
type parkedColumn struct {
	schema, table, column string
	typ                   string // format_type of the column
	constraints           []parkedConstraint
}

// parkedConstraint is a single-column constraint a parked column keeps.
type parkedConstraint struct {
	name, def string
	refRel    string // the table a foreign key references; empty otherwise
}

// parkedAttrsSQL returns a SELECT of (attrelid, attnum, nspname, relname,
// attname) for every parked column outside the tombstone schema: one named
// with the `__parked` suffix, or registered unreaped in atlantis.parked_objects
// when that table exists. It reads whether the table exists.
//
// With restrict, the SELECT covers only the tables whose schemas and names
// the query binds as $1 and $2 text arrays.
func parkedAttrsSQL(ctx context.Context, q introspect.Querier, restrict bool) (string, error) {
	rows, err := q.Query(ctx, `SELECT to_regclass('atlantis.parked_objects') IS NOT NULL`)
	if err != nil {
		return "", err
	}
	registry := false
	for rows.Next() {
		if err := rows.Scan(&registry); err != nil {
			rows.Close()
			return "", err
		}
	}
	if err := rows.Err(); err != nil {
		return "", err
	}

	// The suffix is the name emitFieldRestoreIfParked restores by. A name
	// truncated to 63 bytes loses it, and the registry row still names it.
	sql := `
SELECT a.attrelid, a.attnum, n.nspname, c.relname, a.attname
  FROM pg_attribute a
  JOIN pg_class c ON c.oid = a.attrelid
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE a.attnum > 0 AND NOT a.attisdropped
   AND c.relkind IN ('r', 'p') AND NOT c.relispartition
   AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'pg_toast', '` + codegen.TombstoneSchema + `')`
	if restrict {
		sql += `
   AND (n.nspname, c.relname) IN (SELECT * FROM unnest($1::text[], $2::text[]))`
	}
	sql += `
   AND (right(a.attname, 8) = '__parked'`
	if registry {
		sql += `
        OR EXISTS (SELECT 1 FROM atlantis.parked_objects p
                    WHERE p.kind = 'column' AND p.reaped_at IS NULL AND p.schema_name = n.nspname
                      AND p.parent_table = c.relname AND p.object_name = a.attname)`
	}
	return sql + ")", nil
}

// readParkedColumns returns the managed database's parked columns, each type
// and constraint schema-qualified unless it is in pg_catalog.
func readParkedColumns(ctx context.Context, db introspect.DBTX) ([]parkedColumn, error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	// format_type leaves a type unqualified when the search_path finds it,
	// and the clone's search_path can differ.
	if _, err := tx.Exec(ctx, `SET LOCAL search_path = pg_catalog`); err != nil {
		return nil, err
	}
	parked, err := parkedAttrsSQL(ctx, tx, false)
	if err != nil {
		return nil, err
	}
	// One row per parked column, repeated for each of its single-column
	// UNIQUE, CHECK and FOREIGN KEY constraints.
	rows, err := tx.Query(ctx, `
WITH parked AS (`+parked+`)
SELECT p.nspname::text, p.relname::text, p.attname::text, format_type(a.atttypid, a.atttypmod),
       coalesce(con.conname::text, ''), coalesce(pg_get_constraintdef(con.oid), ''),
       CASE WHEN con.contype = 'f' THEN con.confrelid::regclass::text ELSE '' END
  FROM parked p
  JOIN pg_attribute a ON a.attrelid = p.attrelid AND a.attnum = p.attnum
  LEFT JOIN pg_constraint con
    ON con.conrelid = p.attrelid AND con.conkey = ARRAY[p.attnum]
   AND con.contype IN ('c', 'u', 'f')
 ORDER BY 1, 2, 3, 5`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []parkedColumn
	for rows.Next() {
		var p parkedColumn
		var c parkedConstraint
		if err := rows.Scan(&p.schema, &p.table, &p.column, &p.typ, &c.name, &c.def, &c.refRel); err != nil {
			return nil, err
		}
		if n := len(out); n == 0 || out[n-1].schema != p.schema || out[n-1].table != p.table || out[n-1].column != p.column {
			out = append(out, p)
		}
		if c.name != "" {
			last := &out[len(out)-1]
			last.constraints = append(last.constraints, c)
		}
	}
	return out, rows.Err()
}

// addParkedColumns adds parked to a rehearsal clone built from the checkpoint,
// with their constraints. It leaves out a column whose table or type the
// clone does not have, and a foreign key whose referenced table it does not
// have.
func addParkedColumns(ctx context.Context, tx pgx.Tx, parked []parkedColumn) error {
	if len(parked) == 0 {
		return nil
	}
	rels := make([]string, len(parked))
	types := make([]string, len(parked))
	var refs []string
	for i, p := range parked {
		rels[i] = pgx.Identifier{p.schema, p.table}.Sanitize()
		types[i] = p.typ
		for _, c := range p.constraints {
			if c.refRel != "" {
				refs = append(refs, c.refRel)
			}
		}
	}
	// The setup runs in one transaction, so one missing table or type would
	// fail the whole clone.
	rows, err := tx.Query(ctx, `
SELECT 'column', t.i::text FROM unnest($1::text[], $2::text[]) WITH ORDINALITY AS t(rel, typ, i)
 WHERE to_regclass(t.rel) IS NOT NULL AND to_regtype(t.typ) IS NOT NULL
UNION ALL
SELECT 'ref', r FROM unnest($3::text[]) AS r WHERE to_regclass(r) IS NOT NULL`, rels, types, refs)
	if err != nil {
		return err
	}
	var addable []int
	present := map[string]bool{}
	for rows.Next() {
		var kind, v string
		if err := rows.Scan(&kind, &v); err != nil {
			rows.Close()
			return err
		}
		if kind == "ref" {
			present[v] = true
			continue
		}
		i, err := strconv.Atoi(v)
		if err != nil {
			rows.Close()
			return err
		}
		addable = append(addable, i-1)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	var stmts []string
	for _, i := range addable {
		p := parked[i]
		stmts = append(stmts, "ALTER TABLE "+rels[i]+" ADD COLUMN IF NOT EXISTS "+
			pgx.Identifier{p.column}.Sanitize()+" "+p.typ+";")
		for _, c := range p.constraints {
			if c.refRel != "" && !present[c.refRel] {
				continue
			}
			stmts = append(stmts, "ALTER TABLE "+rels[i]+" ADD CONSTRAINT "+
				pgx.Identifier{c.name}.Sanitize()+" "+c.def+";")
		}
	}
	if len(stmts) == 0 {
		return nil
	}
	_, err = tx.Exec(ctx, strings.Join(stmts, "\n"))
	return err
}
