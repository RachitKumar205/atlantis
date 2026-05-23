// Package introspect reads schema metadata from a live Postgres
// instance and reconstructs a *dsl.IR shaped like what tidectl codegen
// would emit. It is the read-only counterpart to internal/codegen.
//
// v1 scope. The output IR is intentionally partial: only the table /
// column / nullability / primary-key / foreign-key facts are filled in.
// CHECK constraints, multi-column UNIQUEs, and secondary indexes are
// not lowered back into the IR — they would round-trip through several
// canonicalization steps (pg_get_constraintdef adds parens; opclass
// reconstruction is index-method-specific) and the false-drift cost
// outweighs the value for the first cut. Those facts are surfaced as
// advisory warnings instead so the operator can audit them out-of-band.
//
// Atlantis-only metadata (cache, partition_field, query_timeout,
// proto_number ledgers, relations, soft_delete, touch_on_update) has
// no SQL footprint. We copy those values verbatim from the declared
// IR onto the output entity so a downstream diff treats them as equal
// by construction.
//
// Introspection is restricted to the tables the declared IR claims:
// every entity in declaredIR.Entities has a resolved physical
// (schema, table) — either its `table "schema.name"` override or the
// codegen default `atlantis.<ns>_<snake_entity>`. We only query for
// those pairs.
package introspect

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// Querier is the read-only subset of pgxpool.Pool / pgx.Tx introspect
// needs. Accepting an interface lets adopt run introspection inside the
// advisory-locked transaction.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// physRef is the on-disk (schema, table) pair we key every query off
// of. Hoisted to package scope so helpers can share the type.
type physRef struct {
	schema, table string
}

// FromPostgres returns an IR reconstructed from live-DB metadata, the
// set of declared-entity IDs that actually exist in the live DB, and
// advisory warnings (facts we deliberately don't try to verify, and
// security heuristics like un-declared tenant_id columns).
//
// The existing-ID set is used by adopt to filter the IR checkpoint:
// only entities that physically exist get baselined, so subsequent
// `tide plan` / `tide apply` correctly produce CREATE TABLE statements
// for declared-but-not-yet-applied entities.
func FromPostgres(ctx context.Context, q Querier, declaredIR *dsl.IR) (*dsl.IR, map[string]bool, []string, error) {
	if declaredIR == nil {
		return nil, nil, nil, fmt.Errorf("introspect: declaredIR is required")
	}

	idx := make(map[physRef]int, len(declaredIR.Entities))
	pairs := make([]physRef, 0, len(declaredIR.Entities))
	for i, e := range declaredIR.Entities {
		schema, table := physical(&e)
		p := physRef{schema: schema, table: table}
		if _, dup := idx[p]; dup {
			return nil, nil, nil, fmt.Errorf("introspect: declared IR maps two entities to %s.%s", schema, table)
		}
		idx[p] = i
		pairs = append(pairs, p)
	}

	out := &dsl.IR{
		Entities:   make([]dsl.Entity, len(declaredIR.Entities)),
		Queries:    append([]dsl.CustomQuery(nil), declaredIR.Queries...),
		Procedures: append([]dsl.CustomProcedure(nil), declaredIR.Procedures...),
	}
	for i, e := range declaredIR.Entities {
		out.Entities[i] = dsl.Entity{
			Name:               e.Name,
			Namespace:          e.Namespace,
			Kind:               e.Kind,
			TimeField:          e.TimeField,
			TableName:          e.TableName,
			SoftDeleteField:    e.SoftDeleteField,
			TouchOnUpdateField: e.TouchOnUpdateField,
			PartitionField:     e.PartitionField,
			QueryTimeoutMS:     e.QueryTimeoutMS,
			Cache:              cloneCache(e.Cache),
			Relations:          append([]dsl.Relation(nil), e.Relations...),
			Indexes:            append([]dsl.Index(nil), e.Indexes...),
			Uniques:            append([]dsl.UniqueSpec(nil), e.Uniques...),
			Checks:             append([]dsl.TableCheck(nil), e.Checks...),
		}
	}

	existing, err := loadExistingTables(ctx, q, pairs)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load tables: %w", err)
	}
	cols, err := loadColumns(ctx, q, pairs, existing)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load columns: %w", err)
	}
	cons, err := loadConstraints(ctx, q, pairs, existing)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load constraints: %w", err)
	}

	// Lookup tables for FK target reconstruction:
	//   entityByPhys: (schema, table) -> entity id, so introspected refs
	//     can name the declared entity the FK points at.
	//   tableNameByID: entity id -> declared TableName override, so the
	//     introspected Ref carries the same TargetTableName the declared
	//     Ref does. Otherwise refsEqual would compare struct fields and
	//     report drift on every cross-table-override FK.
	entityByPhys := make(map[physRef]string, len(declaredIR.Entities))
	tableNameByID := make(map[string]string, len(declaredIR.Entities))
	for _, e := range declaredIR.Entities {
		s, t := physical(&e)
		entityByPhys[physRef{schema: s, table: t}] = e.ID()
		tableNameByID[e.ID()] = e.TableName
	}

	var warnings []string
	existingIDs := make(map[string]bool, len(declaredIR.Entities))
	for p, ei := range idx {
		oe := &out.Entities[ei]
		de := &declaredIR.Entities[ei]
		if !existing[p] {
			oe.Fields = nil
			warnings = append(warnings, fmt.Sprintf("%s: declared table %s.%s does not exist in the live DB", de.ID(), p.schema, p.table))
			continue
		}
		existingIDs[de.ID()] = true
		assembleEntity(oe, de, cols[p], cons[p], entityByPhys, tableNameByID)
		warnings = append(warnings, partitionWarnings(oe, de)...)
		warnings = append(warnings, unverifiedWarnings(de, cons[p])...)
	}
	sort.Strings(warnings)
	return out, existingIDs, warnings, nil
}

// physical resolves an entity to its on-disk (schema, table) pair.
// Mirrors codegen.entityPhysicalTable without importing it (codegen
// would otherwise reach back into introspect on a future refactor).
func physical(e *dsl.Entity) (schema, table string) {
	if e.TableName == "" {
		return "atlantis", e.Namespace + "_" + snakeCase(e.Name)
	}
	if i := strings.IndexByte(e.TableName, '.'); i > 0 {
		return e.TableName[:i], e.TableName[i+1:]
	}
	return "atlantis", e.TableName
}

func snakeCase(name string) string {
	var b strings.Builder
	b.Grow(len(name) + 4)
	for i, r := range name {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r + ('a' - 'A'))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func cloneCache(c *dsl.Cache) *dsl.Cache {
	if c == nil {
		return nil
	}
	cp := *c
	return &cp
}

func loadExistingTables(ctx context.Context, q Querier, pairs []physRef) (map[physRef]bool, error) {
	out := make(map[physRef]bool, len(pairs))
	for _, p := range pairs {
		out[p] = false
	}
	schemas, tables := splitPairs(pairs)
	rows, err := q.Query(ctx, `
SELECT n.nspname, c.relname
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r','p','f')
  AND (n.nspname, c.relname) IN (SELECT unnest($1::text[]), unnest($2::text[]))`,
		schemas, tables)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var s, t string
		if err := rows.Scan(&s, &t); err != nil {
			return nil, err
		}
		out[physRef{s, t}] = true
	}
	return out, rows.Err()
}

func splitPairs(pairs []physRef) (schemas, tables []string) {
	schemas = make([]string, len(pairs))
	tables = make([]string, len(pairs))
	for i, p := range pairs {
		schemas[i] = p.schema
		tables[i] = p.table
	}
	return
}

func filterExisting(pairs []physRef, existing map[physRef]bool) []physRef {
	out := make([]physRef, 0, len(pairs))
	for _, p := range pairs {
		if existing[p] {
			out = append(out, p)
		}
	}
	return out
}

// colMeta is the raw row shape returned by loadColumns.
type colMeta struct {
	schema, table, name string
	dataType, udtName   string
	charLen, numP, numS int
	hasCharLen, hasNumP bool
	notNull             bool
	defaultExpr         string
	hasDefault          bool
	attIdentity         string
	attGenerated        string
}

func loadColumns(ctx context.Context, q Querier, pairs []physRef, existing map[physRef]bool) (map[physRef][]colMeta, error) {
	out := make(map[physRef][]colMeta, len(pairs))
	live := filterExisting(pairs, existing)
	if len(live) == 0 {
		return out, nil
	}
	schemas, tables := splitPairs(live)
	rows, err := q.Query(ctx, `
SELECT
    n.nspname,
    c.relname,
    a.attname,
    format_type(a.atttypid, a.atttypmod) AS data_type,
    t.typname AS udt_name,
    a.atttypmod,
    a.attnotnull,
    pg_get_expr(d.adbin, d.adrelid) AS default_expr,
    a.attidentity::text,
    a.attgenerated::text
FROM pg_attribute a
JOIN pg_class c ON c.oid = a.attrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_type t ON t.oid = a.atttypid
LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
WHERE NOT a.attisdropped
  AND a.attnum > 0
  AND (n.nspname, c.relname) IN (SELECT unnest($1::text[]), unnest($2::text[]))
ORDER BY n.nspname, c.relname, a.attnum`,
		schemas, tables)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			s, t, name, dt, udt string
			atttypmod           int
			notnull             bool
			defExpr             *string
			attIdent, attGen    string
		)
		if err := rows.Scan(&s, &t, &name, &dt, &udt, &atttypmod, &notnull, &defExpr, &attIdent, &attGen); err != nil {
			return nil, err
		}
		cm := colMeta{
			schema: s, table: t, name: name,
			dataType: dt, udtName: udt,
			notNull:      notnull,
			attIdentity:  attIdent,
			attGenerated: attGen,
		}
		switch udt {
		case "varchar", "bpchar":
			if atttypmod > 4 {
				cm.charLen = atttypmod - 4
				cm.hasCharLen = true
			}
		case "numeric":
			if atttypmod > 0 {
				cm.numP = (atttypmod - 4) >> 16
				cm.numS = (atttypmod - 4) & 0xffff
				cm.hasNumP = true
			}
		}
		if defExpr != nil {
			cm.defaultExpr = *defExpr
			cm.hasDefault = true
		}
		key := physRef{s, t}
		out[key] = append(out[key], cm)
	}
	return out, rows.Err()
}

type constraints struct {
	pk     []string
	fks    []fkSpec
	uniqs  []uniqSpec
	checks []checkSpec
}
type fkSpec struct {
	col, tgtSchema, tgtTable, tgtCol string
	onDelete, onUpdate               string
}
type uniqSpec struct{ cols []string }
type checkSpec struct{ expr string }

func loadConstraints(ctx context.Context, q Querier, pairs []physRef, existing map[physRef]bool) (map[physRef]constraints, error) {
	out := make(map[physRef]constraints, len(pairs))
	live := filterExisting(pairs, existing)
	if len(live) == 0 {
		return out, nil
	}
	schemas, tables := splitPairs(live)
	rows, err := q.Query(ctx, `
WITH targets AS (
    SELECT unnest($1::text[]) AS schema, unnest($2::text[]) AS table_name
)
SELECT
    n.nspname,
    c.relname,
    con.contype::text,
    con.confdeltype::text,
    con.confupdtype::text,
    pg_get_constraintdef(con.oid, true) AS def,
    fn.nspname  AS ftarget_schema,
    fc.relname  AS ftarget_table,
    (SELECT array_agg(att.attname ORDER BY array_position(con.conkey, att.attnum))
       FROM pg_attribute att
      WHERE att.attrelid = con.conrelid
        AND att.attnum = ANY(con.conkey)) AS col_names,
    (SELECT array_agg(att.attname ORDER BY array_position(con.confkey, att.attnum))
       FROM pg_attribute att
      WHERE att.attrelid = con.confrelid
        AND con.confkey IS NOT NULL
        AND att.attnum = ANY(con.confkey)) AS fcol_names
FROM pg_constraint con
JOIN pg_class c ON c.oid = con.conrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_class fc ON fc.oid = con.confrelid
LEFT JOIN pg_namespace fn ON fn.oid = fc.relnamespace
JOIN targets tg ON tg.schema = n.nspname AND tg.table_name = c.relname
WHERE con.contype IN ('p','f','u','c')`,
		schemas, tables)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			s, t                  string
			contype, ondel, onupd string
			def                   string
			ftgtSchema, ftgtTable *string
			colNames, fcolNames   []string
		)
		if err := rows.Scan(&s, &t, &contype, &ondel, &onupd, &def, &ftgtSchema, &ftgtTable, &colNames, &fcolNames); err != nil {
			return nil, err
		}
		key := physRef{s, t}
		c := out[key]
		switch contype {
		case "p":
			c.pk = colNames
		case "f":
			fk := fkSpec{
				onDelete: actionFromCode(ondel),
				onUpdate: actionFromCode(onupd),
			}
			if ftgtSchema != nil {
				fk.tgtSchema = *ftgtSchema
			}
			if ftgtTable != nil {
				fk.tgtTable = *ftgtTable
			}
			if len(colNames) == 1 {
				fk.col = colNames[0]
			}
			if len(fcolNames) == 1 {
				fk.tgtCol = fcolNames[0]
			}
			c.fks = append(c.fks, fk)
		case "u":
			c.uniqs = append(c.uniqs, uniqSpec{cols: colNames})
		case "c":
			expr := strings.TrimPrefix(def, "CHECK ")
			expr = strings.TrimSpace(expr)
			if strings.HasPrefix(expr, "(") && strings.HasSuffix(expr, ")") {
				expr = expr[1 : len(expr)-1]
			}
			c.checks = append(c.checks, checkSpec{expr: strings.TrimSpace(expr)})
		}
		out[key] = c
	}
	return out, rows.Err()
}

func actionFromCode(code string) string {
	switch code {
	case "a":
		return "NO ACTION"
	case "r":
		return "RESTRICT"
	case "c":
		return "CASCADE"
	case "n":
		return "SET NULL"
	case "d":
		return "SET DEFAULT"
	}
	return ""
}

// refActionFromKeyword maps the long-form FK action string back onto the
// IR enum. "NO ACTION" and "SET DEFAULT" map to Unset — the IR doesn't
// distinguish them (the declared side has no spelling for either).
func refActionFromKeyword(kw string) dsl.RefAction {
	switch kw {
	case "CASCADE":
		return dsl.RefActionCascade
	case "RESTRICT":
		return dsl.RefActionRestrict
	case "SET NULL":
		return dsl.RefActionSetNull
	}
	return dsl.RefActionUnset
}

// assembleEntity fills the SQL-derived parts of `out` from the raw fact
// rows. Field ordering follows declared.Fields where possible; columns
// present in the DB but not declared are appended afterwards so the
// diff still flags them (KindFieldAdded from declared perspective).
func assembleEntity(out, declared *dsl.Entity, cols []colMeta, cons constraints, entityByPhys map[physRef]string, tableNameByID map[string]string) {
	byName := make(map[string]colMeta, len(cols))
	for _, c := range cols {
		byName[c.name] = c
	}
	declaredNames := make(map[string]bool, len(declared.Fields))
	for _, df := range declared.Fields {
		declaredNames[df.Name] = true
	}

	pkSet := make(map[string]bool, len(cons.pk))
	for _, c := range cons.pk {
		pkSet[c] = true
	}
	if len(cons.pk) > 1 {
		out.CompositePK = append([]string(nil), cons.pk...)
	}

	uniqColSet := make(map[string]bool)
	for _, u := range cons.uniqs {
		if len(u.cols) == 1 {
			uniqColSet[u.cols[0]] = true
		}
	}

	fkByCol := make(map[string]fkSpec, len(cons.fks))
	for _, f := range cons.fks {
		if f.col != "" {
			fkByCol[f.col] = f
		}
	}

	emit := func(c colMeta) dsl.Field {
		f := dsl.Field{
			Name:    c.name,
			Type:    fieldType(c),
			NotNull: c.notNull,
		}
		if len(cons.pk) == 1 && cons.pk[0] == c.name {
			f.Primary = true
		}
		if uniqColSet[c.name] {
			f.Unique = true
		}
		switch {
		case c.attIdentity == "a" || c.attIdentity == "d":
			f.Identity = true
		case c.hasDefault && isSerialDefault(c.defaultExpr):
			f.Serial = true
		case c.hasDefault:
			f.Default = parseDefault(c.defaultExpr)
		}
		if fk, ok := fkByCol[c.name]; ok {
			tgtKey := physRef{schema: fk.tgtSchema, table: fk.tgtTable}
			tgtID := entityByPhys[tgtKey]
			f.Ref = &dsl.Ref{
				TargetID:        tgtID,
				TargetField:     fk.tgtCol,
				TargetTableName: tableNameByID[tgtID],
				OnDelete:        refActionFromKeyword(fk.onDelete),
				OnUpdate:        refActionFromKeyword(fk.onUpdate),
			}
		}
		return f
	}

	for _, df := range declared.Fields {
		if c, ok := byName[df.Name]; ok {
			out.Fields = append(out.Fields, emit(c))
		}
	}
	for _, c := range cols {
		if declaredNames[c.name] {
			continue
		}
		out.Fields = append(out.Fields, emit(c))
	}
}

// fieldType produces a dsl.FieldType from one column row. We canonicalize
// Postgres UDT names to the .atl-side spellings the IR uses.
//
// Array detection: Postgres stores array types with udtName "_<elem>"
// (e.g., "_text", "_int4"). We translate to the .atl shape
// `{Name: elem, Array: true, Elem: {Name: elem}}` so the diff against
// a declared `text[]` field matches.
func fieldType(c colMeta) dsl.FieldType {
	if strings.HasPrefix(c.udtName, "_") {
		elemUDT := c.udtName[1:]
		elem := canonicalUDT(elemUDT)
		return dsl.FieldType{
			Name:  elem,
			Array: true,
			Elem:  &dsl.FieldType{Name: elem},
		}
	}
	ft := dsl.FieldType{Name: canonicalUDT(c.udtName)}
	if c.hasCharLen {
		ft.Len = c.charLen
	}
	if c.hasNumP {
		ft.NumP = c.numP
		ft.NumS = c.numS
		ft.HasNumP = true
	}
	if c.udtName == "vector" {
		ft.VecDim = parseVectorDim(c.dataType)
	}
	return ft
}

// canonicalUDT maps Postgres internal type names to the .atl spellings.
// Anything unrecognized passes through verbatim — diff treats verbatim
// names as opaque strings, which is the right behavior for unknown
// types that may appear in the legacy schema.
func canonicalUDT(udt string) string {
	switch udt {
	case "int2":
		return "smallint"
	case "int4":
		return "int"
	case "int8":
		return "bigint"
	case "float4":
		return "real"
	case "float8":
		return "double precision"
	case "bool":
		return "boolean"
	case "bpchar":
		return "char"
	case "timestamptz":
		return "timestamptz"
	case "timestamp":
		return "timestamp"
	}
	return udt
}

func parseVectorDim(s string) int {
	i := strings.IndexByte(s, '(')
	j := strings.LastIndexByte(s, ')')
	if i < 0 || j < 0 || j <= i+1 {
		return 0
	}
	n := 0
	for _, r := range s[i+1 : j] {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func isSerialDefault(expr string) bool {
	return strings.HasPrefix(strings.TrimSpace(expr), "nextval(")
}

func parseDefault(expr string) *dsl.Default {
	trimmed := strings.TrimSpace(expr)
	low := strings.ToLower(trimmed)
	switch low {
	case "now()", "current_timestamp", "transaction_timestamp()":
		return &dsl.Default{Kind: dsl.DefaultIRNow}
	case "true":
		return &dsl.Default{Kind: dsl.DefaultIRBool, Bool: true}
	case "false":
		return &dsl.Default{Kind: dsl.DefaultIRBool, Bool: false}
	}
	// String literal: only collapse to DefaultIRString when the implicit
	// cast targets a string type ('foo'::text, 'foo'::varchar, or no
	// cast). Anything else — `'[]'::jsonb`, `'{"k":"v"}'::jsonb`, an enum
	// cast — round-trips verbatim as raw, matching how `default raw "…"`
	// lowers on the declared side.
	if s, castTarget, ok := stripStringLiteral(trimmed); ok {
		if isStringCastTarget(castTarget) {
			return &dsl.Default{Kind: dsl.DefaultIRString, Str: s}
		}
		return &dsl.Default{Kind: dsl.DefaultIRRaw, Str: trimmed}
	}
	if n, ok := tryParseInt(trimmed); ok {
		return &dsl.Default{Kind: dsl.DefaultIRInt, Int: n}
	}
	// pg_get_expr renders NUMERIC literals with their declared precision
	// (`0.00` for `default 0` against a NUMERIC(5,2)). When the fractional
	// part is all zeros the value is integer-equivalent — match the
	// declared `default 0` so it doesn't false-drift.
	if n, ok := tryParseDecimalAsInt(trimmed); ok {
		return &dsl.Default{Kind: dsl.DefaultIRInt, Int: n}
	}
	return &dsl.Default{Kind: dsl.DefaultIRRaw, Str: trimmed}
}

// tryParseDecimalAsInt parses strings like "0.00", "5.0", "-3.000" as
// the integer they're numerically equal to. Returns false when the
// fractional part is non-zero or the input is malformed.
func tryParseDecimalAsInt(s string) (int64, bool) {
	dot := strings.IndexByte(s, '.')
	if dot < 0 {
		return 0, false
	}
	for _, r := range s[dot+1:] {
		if r != '0' {
			return 0, false
		}
	}
	return tryParseInt(s[:dot])
}

// isStringCastTarget reports whether the cast suffix on a quoted Postgres
// literal merely re-asserts the column's own string type. pg_get_expr
// renders the cast using the canonical type name (e.g. `'active'::character varying`
// for a varchar column, `'foo'::text` for text), so we have to recognize
// every variant Postgres might produce.
func isStringCastTarget(t string) bool {
	switch t {
	case "", "text", "bpchar", "character", "character varying", "name":
		return true
	}
	if strings.HasPrefix(t, "varchar") || strings.HasPrefix(t, "character varying") || strings.HasPrefix(t, "character(") {
		return true
	}
	return false
}

// stripStringLiteral parses one quoted Postgres literal with an optional
// "::cast" suffix. Returns (body, castTarget, true) when the input is
// well-formed; the body has doubled single quotes collapsed. castTarget
// is the lowercased cast type with parameters stripped of whitespace
// (e.g., "text", "jsonb", "varchar(255)"); empty when no cast was
// present.
func stripStringLiteral(s string) (body, castTarget string, ok bool) {
	if len(s) < 2 || s[0] != '\'' {
		return "", "", false
	}
	end := -1
	for i := 1; i < len(s); i++ {
		if s[i] != '\'' {
			continue
		}
		if i+1 < len(s) && s[i+1] == '\'' {
			i++
			continue
		}
		end = i
		break
	}
	if end < 0 {
		return "", "", false
	}
	body = strings.ReplaceAll(s[1:end], "''", "'")
	tail := strings.TrimSpace(s[end+1:])
	if tail == "" {
		return body, "", true
	}
	if strings.HasPrefix(tail, "::") {
		return body, strings.ToLower(strings.TrimSpace(tail[2:])), true
	}
	return "", "", false
}

func tryParseInt(s string) (int64, bool) {
	if len(s) == 0 {
		return 0, false
	}
	var n int64
	neg := false
	i := 0
	if s[0] == '-' {
		neg = true
		i++
	}
	if i == len(s) {
		return 0, false
	}
	for ; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
		n = n*10 + int64(s[i]-'0')
	}
	if neg {
		n = -n
	}
	return n, true
}

func partitionWarnings(out, declared *dsl.Entity) []string {
	if declared.PartitionField != "" {
		return nil
	}
	var warns []string
	for _, f := range out.Fields {
		switch f.Name {
		case "tenant_id", "org_id", "account_id", "workspace_id":
			warns = append(warns, fmt.Sprintf("%s: column %q is present in the live DB but the entity has no `partition_field %s` — adopt will baseline as un-partitioned (potential cross-tenant exposure)", declared.ID(), f.Name, f.Name))
		}
	}
	return warns
}

// unverifiedWarnings records facts adopt deliberately doesn't lift back
// into IR. The operator sees them in the report so they know the diff
// is silent on these specific axes.
func unverifiedWarnings(declared *dsl.Entity, cons constraints) []string {
	var warns []string
	if n := len(declared.Indexes); n > 0 {
		warns = append(warns, fmt.Sprintf("%s: %d declared index(es) not verified against the live DB (adopt v1 covers tables, columns, PKs, FKs only)", declared.ID(), n))
	}
	if n := len(declared.Checks); n > 0 || len(cons.checks) > 0 {
		warns = append(warns, fmt.Sprintf("%s: %d declared CHECK constraint(s), %d live CHECK constraint(s) — not verified", declared.ID(), len(declared.Checks), len(cons.checks)))
	}
	if n := len(declared.Uniques); n > 0 || len(cons.uniqs) > 0 {
		// Single-column uniqueness is already verified above via Field.Unique;
		// only multi-column constraints land here.
		warns = append(warns, fmt.Sprintf("%s: %d declared composite UNIQUE(s), %d live composite UNIQUE(s) — not verified", declared.ID(), n, multiColUniqCount(cons.uniqs)))
	}
	return warns
}

func multiColUniqCount(us []uniqSpec) int {
	n := 0
	for _, u := range us {
		if len(u.cols) > 1 {
			n++
		}
	}
	return n
}
