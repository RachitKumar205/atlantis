// Package introspect reads schema metadata from a live Postgres
// instance and reconstructs a *dsl.IR shaped like what tidectl codegen
// would emit. It is the read-only counterpart to internal/codegen.
//
// The output IR is partial. Table, column, nullability, primary-key and
// foreign-key facts are filled in; CHECK constraints, multi-column UNIQUEs and
// secondary indexes are surfaced as advisory warnings rather than lowered,
// because round-tripping them produces false drift — pg_get_constraintdef adds
// parens, and opclass reconstruction is index-method-specific.
//
// Atlantis-only metadata — cache, query_timeout, proto_number ledgers,
// relations, soft_delete, touch_on_update — has no SQL footprint, so those
// values are copied verbatim from the declared IR onto the output entity and a
// downstream diff treats them as equal by construction.
//
// partition_field is not in that set. It has a SQL footprint, ENABLE and FORCE
// ROW LEVEL SECURITY plus a policy, and copying it verbatim would make the live
// IR agree with the schema by construction — leaving `tide adopt` unable to
// report a table whose isolation the database is not enforcing. It is read from
// pg_policies; see loadPartitionPolicies.
//
// Only the tables the declared IR claims are queried. Every entity in
// declaredIR.Entities resolves to a physical (schema, table) pair, from its
// `table "schema.name"` override or the codegen default
// `atlantis.<ns>_<snake_entity>`.
package introspect

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/schema"
)

// Querier is the read-only subset of pgxpool.Pool / pgx.Tx introspect
// needs. Accepting an interface lets adopt run introspection inside the
// advisory-locked transaction.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// physRef is the on-disk (schema, table) pair every query is keyed by. At
// package scope so the helpers share the type.
type physRef struct {
	schema, table string
}

// FromPostgres returns an IR reconstructed from live-DB metadata, the
// set of declared-entity IDs that actually exist in the live DB, and
// advisory warnings — facts this does not verify, plus security heuristics
// such as an undeclared tenant_id column.
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

	// Enum types as the database has them. Without this every declared enum
	// reads as removed, because the live IR would carry none.
	liveEnums, err := loadLiveEnums(ctx, q, declaredIR.Enums)
	if err != nil {
		return nil, nil, nil, err
	}
	out.Enums = liveEnums
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

	// Tenant isolation, read from the catalog rather than copied off the
	// declaration.
	//
	// PartitionField was assigned from the declared entity above, which made
	// the live IR agree with the schema by construction: oldE.PartitionField
	// always equalled newE.PartitionField, diffPartition returned on its first
	// line, and `partition_added` / `partition_removed` could never reach the
	// drift report. A review confirmed it by stripping the policy, FORCE and
	// RLS off a live table whose declaration said `partition by tenant` —
	// introspection reported the column as partitioned, with no warning, and
	// the diff was empty.
	//
	// That is the one drift this feature most needs to surface. `tide adopt`
	// exists to tell an operator what the database really looks like, and on
	// this question it was telling them what the file said.
	live, err := loadPartitionPolicies(ctx, q, pairs, existing)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load partition policies: %w", err)
	}
	for p, i := range idx {
		if !existing[p] {
			continue
		}
		out.Entities[i].PartitionField = live[p]
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
		warnings = append(warnings, promotedKeyWarnings(de, cols[p], cons[p])...)
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
	declaredChecks := make(map[string]string, len(declared.Fields))
	for _, df := range declared.Fields {
		declaredNames[df.Name] = true
		if df.Check != "" {
			declaredChecks[df.Name] = df.Check
		}
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

	promotedKey := promotableKey(cols, cons)

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
			// Carried from the declaration rather than read out of
			// pg_constraint, matching how entity-level Checks are handled at
			// the top of FromPostgres. This introspection supplies column
			// facts; CHECK predicates it does not verify, and unverifiedWarnings
			// says so.
			//
			// Carrying it is what keeps that honest. Leaving it empty made both
			// sides differ by construction, so every adopt of a database whose
			// CHECK constraints already existed reported them as drift to be
			// added — a permanent false positive contradicting the warning
			// emitted alongside it. Comparing an author's `total > 0` against
			// Postgres's normalised `((total > 0))` is the real fix and needs
			// expression normalisation, not a string compare.
			Check: declaredChecks[c.name],
		}
		if (len(cons.pk) == 1 && cons.pk[0] == c.name) || c.name == promotedKey {
			f.Primary = true
		}
		if uniqColSet[c.name] {
			f.Unique = true
		}
		switch {
		case c.attIdentity == "a" || c.attIdentity == "d":
			f.Identity = true
		// A sequence default on a NOT NULL column. Postgres's SERIAL is an
		// integer, a sequence and NOT NULL together, and lowering reads
		// `serial` back as all three — so calling a nullable column with a
		// nextval default `serial` declares a NOT NULL the database does not
		// have, and the next plan proposes tightening it.
		//
		// The nullable form falls through and keeps the default verbatim,
		// which is what the column actually is.
		case c.hasDefault && c.notNull && isSerialDefault(c.defaultExpr):
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
//
// Every name this returns is compared as a raw string against what a user
// declared, so returning anything other than the exact .atl spelling makes a
// column permanently un-matchable. "double precision" was that mistake.
//
// Anything unrecognized passes through verbatim — diff treats verbatim names
// as opaque strings, which is the right behavior for unknown types that may
// appear in the legacy schema. bpchar→char is in that group rather than a
// translation: .atl has no char(N), so the mapping only makes a drift report
// readable and cannot make the column matchable.
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
		// "double", not "double precision". The .atl spelling is one token
		// because the parser reads a type as one identifier, and this function
		// is defined as producing .atl spellings — a name only reaches typeEqual
		// after passing through here, so returning the SQL spelling meant a
		// float8 column could never equal any declaration a user could write.
		// `tide inspect` reported a permanent mismatch and adopt refused to
		// baseline without --allow-drift, on a schema that was correct.
		return "double"
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
// renders the cast under the canonical type name — `'active'::character
// varying` for a varchar column, `'foo'::text` for text — so every variant
// Postgres produces is listed below.
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

// unverifiedWarnings records the facts adopt does not lift back into IR. They
// appear in the report, so the axes the diff is silent on are named.
func unverifiedWarnings(declared *dsl.Entity, cons constraints) []string {
	var warns []string
	if n := len(declared.Indexes); n > 0 {
		warns = append(warns, fmt.Sprintf("%s: %d declared index(es) not verified against the live DB (adopt v1 covers tables, columns, PKs, FKs only)", declared.ID(), n))
	}
	// Field-level `check` modifiers count toward "declared", not just
	// entity-level ones. Counting only declared.Checks made the warning silent
	// for a schema whose checks are all per-field — and since assembleEntity
	// carries Field.Check across from the declaration, that is exactly the case
	// where the diff is clean by construction. A table whose CHECK had been
	// dropped by hand produced no drift item and no warning: nothing at all.
	declaredChecks := len(declared.Checks)
	for i := range declared.Fields {
		if declared.Fields[i].Check != "" {
			declaredChecks++
		}
	}
	if declaredChecks > 0 || len(cons.checks) > 0 {
		warns = append(warns, fmt.Sprintf("%s: %d declared CHECK constraint(s), %d live CHECK constraint(s) — not verified", declared.ID(), declaredChecks, len(cons.checks)))
	}
	if n := len(declared.Uniques); n > 0 || len(cons.uniqs) > 0 {
		// Single-column uniqueness is already verified above via Field.Unique;
		// only multi-column constraints land here.
		warns = append(warns, fmt.Sprintf("%s: %d declared composite UNIQUE(s), %d live composite UNIQUE(s) — not verified", declared.ID(), n, multiColUniqCount(cons.uniqs)))
	}
	return warns
}

// promotableKey names the column an entity is addressed by when its table has
// no PRIMARY KEY: a single-column UNIQUE constraint whose column is NOT NULL.
//
// PRIMARY KEY is UNIQUE and NOT NULL together, so such a column addresses a row
// exactly as a declared key would. Postgres refuses a foreign key to a table
// with no unique constraint — `there is no unique constraint matching given
// keys for referenced table` — so a table any foreign key points at always has
// a candidate here.
//
// Empty when the table has a PRIMARY KEY, when no unique constraint covers a
// single column, or when the column it covers is nullable. Ties go to the
// lowest attnum, which is the order cols arrives in, so the choice does not
// move between reads of the same table.
func promotableKey(cols []colMeta, cons constraints) string {
	if len(cons.pk) > 0 {
		return ""
	}
	single := make(map[string]bool, len(cons.uniqs))
	for _, u := range cons.uniqs {
		if len(u.cols) == 1 {
			single[u.cols[0]] = true
		}
	}
	for _, c := range cols {
		if single[c.name] && c.notNull {
			return c.name
		}
	}
	return ""
}

// promotedKeyWarnings reports a key taken from a UNIQUE constraint.
//
// The declaration says `primary` where the catalogue says UNIQUE NOT NULL. The
// two describe the same addressability and differ in what a fresh CREATE TABLE
// would emit, so the choice is named rather than left for a reader to infer
// from a table that has no primary key and a declaration that claims one.
func promotedKeyWarnings(declared *dsl.Entity, cols []colMeta, cons constraints) []string {
	key := promotableKey(cols, cons)
	if key == "" {
		return nil
	}
	return []string{fmt.Sprintf(
		"%s: no primary key; column %q is addressed as the key because it is "+
			"NOT NULL and UNIQUE, which is what a primary key is. Applying this "+
			"declaration to a new database creates it as PRIMARY KEY",
		declared.ID(), key)}
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

// loadPartitionPolicies reports, per table, which column the live database
// actually enforces tenant isolation on — "" when it enforces none.
//
// Enforced means all three of: row-level security ENABLED, FORCED, and a
// permissive policy
// whose USING expression compares a column to atlantis.current_partition().
// Any one missing and the table is not isolated, whatever the schema says:
//
//   - not ENABLED, the policy is inert
//   - not FORCED, the table's owner is exempt, and atlantis owns the tables it
//     creates and connects as their owner
//   - no policy, nothing filters
//
// The column is read from the predicate through pg_get_expr. Returning a
// boolean and reusing the declared name reports a policy on the wrong column as
// correct — reachable because moving `partition by` to another column is a
// change the database does not apply by itself. Read this way, a policy scoping
// an undeclared column surfaces as drift.
//
// A policy this cannot parse yields "", reported as not isolated. The plan then
// emits DROP POLICY IF EXISTS followed by CREATE POLICY, converging a
// hand-written policy onto the declared one; the other default accepts it
// unread.
func loadPartitionPolicies(ctx context.Context, q Querier, pairs []physRef, existing map[physRef]bool) (map[physRef]string, error) {
	out := make(map[physRef]string, len(pairs))
	schemas := make([]string, 0, len(pairs))
	tables := make([]string, 0, len(pairs))
	for _, p := range pairs {
		if !existing[p] {
			continue
		}
		schemas = append(schemas, p.schema)
		tables = append(tables, p.table)
	}
	if len(schemas) == 0 {
		return out, nil
	}

	// EVERY policy, ordered, permissive and restrictive alike — the boundary is
	// restrictive and the grants are permissive, and this has to tell them
	// apart.
	//
	// It read only permissive policies when the boundary lived there, and
	// required every one of them to scope the column, because permissive
	// policies OR and a single unscoped one beside a correct one returns every
	// tenant's rows. With the boundary restrictive that requirement is not just
	// unnecessary but wrong: a user's access-control grants are permissive by
	// nature and cannot widen past a restrictive policy, so demanding they
	// mention the tenant column would report every RBAC-carrying table as
	// broken.
	//
	// pg.VerifyPartitionPolicies asks the same question ten lines of import
	// away. The two must agree, or `tide adopt` reports a table healthy that
	// the server then refuses to serve.
	// pg_policies, not pg_policy. The catalogue table is one of the few
	// PostgreSQL withholds from an ordinary role, so reading it fails the whole
	// import against a database whose owner did not hand out superuser —
	// measured: `permission denied for table pg_policy (SQLSTATE 42501)` on a
	// public read-only dataset. The view carries the same rows already
	// deparsed, and is granted to PUBLIC.
	rows, err := q.Query(ctx, `
SELECT p.schemaname, p.tablename,
       coalesce(p.qual, ''),
       coalesce(p.with_check, ''),
       p.cmd,
       p.roles = '{public}'::name[],
       p.permissive = 'PERMISSIVE'
  FROM pg_policies p
  JOIN pg_namespace n ON n.nspname = p.schemaname
  JOIN pg_class c ON c.relname = p.tablename AND c.relnamespace = n.oid
 WHERE (p.schemaname, p.tablename) IN (SELECT * FROM unnest($1::text[], $2::text[]))
   AND c.relrowsecurity
   AND c.relforcerowsecurity
 ORDER BY p.schemaname, p.tablename, p.policyname`, schemas, tables)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type state struct {
		cols          map[string]bool // columns scoped by restrictive read policies
		reads, writes int             // restrictive policies that scope, by half
		grants        int             // permissive policies of any shape
		toOther       bool            // a boundary granted to some other role
	}
	seen := map[physRef]*state{}
	for rows.Next() {
		var schema, table, qual, withCheck, cmd string
		var toPublic, permissive bool
		if err := rows.Scan(&schema, &table, &qual, &withCheck, &cmd, &toPublic, &permissive); err != nil {
			return nil, err
		}
		key := physRef{schema: schema, table: table}
		st := seen[key]
		if st == nil {
			st = &state{cols: map[string]bool{}}
			seen[key] = st
		}

		// Permissive policies are grants. Their shape is the user's business —
		// they cannot reach outside a restrictive boundary however they are
		// written. Only their EXISTENCE matters here, because a table with no
		// grant admits nothing.
		if permissive {
			st.grants++
			continue
		}

		// A boundary granted only to some other role applies to nobody on the
		// connecting role, so it bounds nothing.
		if !toPublic {
			st.toOther = true
			continue
		}
		if governsReadCmd(cmd) {
			if col := partitionColumnFromQual(qual); col != "" {
				st.cols[col] = true
				st.reads++
			}
		}
		if governsWriteCmd(cmd) {
			// For ALL and UPDATE a null WITH CHECK reuses USING as the check.
			check := withCheck
			if check == "" {
				check = qual
			}
			if col := partitionColumnFromQual(check); col != "" {
				st.cols[col] = true
				st.writes++
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for key, st := range seen {
		// Both halves bounded, at least one grant so the table is reachable,
		// and exactly one column named. More than one is not "more isolated" to
		// report — restrictive policies AND, so two of them on different columns
		// genuinely both apply, but there is no single answer to "which column
		// is the partition column" and inventing one would put a wrong value in
		// the live IR.
		if st.toOther || st.reads == 0 || st.writes == 0 || st.grants == 0 || len(st.cols) != 1 {
			continue
		}
		for col := range st.cols {
			out[key] = col
		}
	}
	return out, nil
}

// governsReadCmd and governsWriteCmd classify a policy's command.
//
// The spelling is pg_policies.cmd — ALL, SELECT, INSERT, UPDATE, DELETE —
// rather than pg_policy.polcmd's '*rawd', because the view is what an ordinary
// role may read.
//
// UPDATE and DELETE have a USING half, so they decide which rows are reachable;
// UPDATE and INSERT have a WITH CHECK half, so they decide what may be written.
func governsReadCmd(cmd string) bool {
	return cmd == "ALL" || cmd == "SELECT" || cmd == "UPDATE" || cmd == "DELETE"
}

func governsWriteCmd(cmd string) bool {
	return cmd == "ALL" || cmd == "INSERT" || cmd == "UPDATE"
}

// partitionColumnFromQual extracts the column an isolation predicate scopes.
//
// The emitted shape is `(<col> = atlantis.current_partition()[::cast])`, which
// pg_get_expr renders with the schema stripped and the column possibly cast:
//
//	((tenant)::text = NULLIF(current_setting('atlantis.tenant'::text, true), ''::text))
//	(tenant = current_partition())
//
// Anything that does not mention current_partition or current_setting of the
// tenant parameter is not an isolation policy and yields "". Anything that does
// but cannot be parsed also yields "", which reports as "not isolated" — see
// loadPartitionPolicies for why that direction is the safe one.
func partitionColumnFromQual(qual string) string {
	if !strings.Contains(qual, "current_partition") &&
		!strings.Contains(qual, "atlantis.tenant") {
		return ""
	}
	eq := strings.Index(qual, " = ")
	if eq < 0 {
		return ""
	}
	lhs := strings.TrimSpace(qual[:eq])
	// Strip the wrapping parens and any cast pg_get_expr added: `((tenant)::text`
	lhs = strings.TrimLeft(lhs, "(")
	if cast := strings.Index(lhs, ")::"); cast >= 0 {
		lhs = lhs[:cast]
	}
	// A CALL on the left is not a column, and trimming its parens away would
	// turn it into one. `(current_partition() = tenant)` — the operands the
	// other way round, entirely plausible in a hand-written policy — used to
	// yield the string "current_partition" as the live partition column. The
	// operator was then shown "tenant isolation moved from "current_partition"
	// to "tenant"", and the down script would have created a policy against a
	// column that does not exist.
	if strings.Contains(lhs, "(") {
		return ""
	}
	lhs = strings.Trim(lhs, `") `)
	// A qualified or expression left-hand side is not a plain column.
	if lhs == "" || strings.ContainsAny(lhs, ". ()'") {
		return ""
	}
	return lhs
}

// loadLiveEnums reads the labels of the declared enum types that exist.
//
// Keyed by the flattened name schema.EnumTypeName produces, so the lookup is
// exact. Recovering a declared `namespace.Name` from an arbitrary live type
// name is not possible — `rnacen.status` could be namespace `rnacen` and name
// `status` or a namespace called `rnacen_status` — so an enum atlantis did not
// create is not read here. Adopting one needs a way to write the live type name
// in the declaration.
//
// A declared enum with no live type is omitted, so the diff reports it as added
// rather than as a mismatch.
func loadLiveEnums(ctx context.Context, q Querier, declared []dsl.Enum) ([]dsl.Enum, error) {
	if len(declared) == 0 {
		return nil, nil
	}
	byTypeName := make(map[string]*dsl.Enum, len(declared))
	names := make([]string, 0, len(declared))
	for i := range declared {
		tn := schema.EnumTypeName(&declared[i])
		byTypeName[tn] = &declared[i]
		names = append(names, tn)
	}

	rows, err := q.Query(ctx, `
SELECT t.typname, e.enumlabel
FROM pg_type t
JOIN pg_namespace n ON n.oid = t.typnamespace
JOIN pg_enum e ON e.enumtypid = t.oid
WHERE t.typtype = 'e'
  AND n.nspname = 'atlantis'
  AND t.typname = ANY($1)
ORDER BY t.typname, e.enumsortorder`, names)
	if err != nil {
		return nil, fmt.Errorf("introspect enums: %w", err)
	}
	defer rows.Close()

	labels := map[string][]string{}
	for rows.Next() {
		var typname, label string
		if err := rows.Scan(&typname, &label); err != nil {
			return nil, fmt.Errorf("introspect enums: %w", err)
		}
		labels[typname] = append(labels[typname], label)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("introspect enums: %w", err)
	}

	var out []dsl.Enum
	for _, tn := range names {
		vals, ok := labels[tn]
		if !ok {
			continue
		}
		d := byTypeName[tn]
		out = append(out, dsl.Enum{Name: d.Name, Namespace: d.Namespace, Values: vals})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out, nil
}
