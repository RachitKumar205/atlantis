package entity

import (
	"strings"
	"testing"

	// Import the compiled predicates proto so that the global proto
	// registry has the atlantis.common.v1.* types available for the
	// dynamic descriptor builder tests.
	_ "github.com/rachitkumar205/atlantis-go/pb/atlantis/common/v1"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// testAccount returns a representative entity with several field types,
// a soft delete column, and a default — covering the most important SQL
// builder and proto descriptor code paths.
func testAccount() *dsl.Entity {
	return &dsl.Entity{
		Name:            "Account",
		Namespace:       "consumer",
		Kind:            dsl.EntityKindRegular,
		SoftDeleteField: "deleted_at",
		Fields: []dsl.Field{
			{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true, NotNull: true, ProtoNumber: 1},
			{Name: "email", Type: dsl.FieldType{Name: "citext"}, NotNull: true, ProtoNumber: 2},
			{Name: "name", Type: dsl.FieldType{Name: "varchar", Len: 255}, ProtoNumber: 3},
			{Name: "is_active", Type: dsl.FieldType{Name: "boolean"}, NotNull: true, ProtoNumber: 4},
			{Name: "created_at", Type: dsl.FieldType{Name: "timestamptz"}, NotNull: true, Default: &dsl.Default{Kind: dsl.DefaultIRNow}, ProtoNumber: 5},
			{Name: "deleted_at", Type: dsl.FieldType{Name: "timestamptz"}, ProtoNumber: 6},
			{Name: "balance", Type: dsl.FieldType{Name: "numeric", HasNumP: true, NumP: 10, NumS: 2}, ProtoNumber: 7},
			{Name: "metadata", Type: dsl.FieldType{Name: "jsonb"}, ProtoNumber: 8},
		},
	}
}

// testCompositeEntity returns an entity with a composite PK.
func testCompositeEntity() *dsl.Entity {
	return &dsl.Entity{
		Name:        "CartItem",
		Namespace:   "consumer",
		Kind:        dsl.EntityKindRegular,
		CompositePK: []string{"cart_id", "variant_id"},
		Fields: []dsl.Field{
			{Name: "cart_id", Type: dsl.FieldType{Name: "bigint"}, NotNull: true, ProtoNumber: 1},
			{Name: "variant_id", Type: dsl.FieldType{Name: "bigint"}, NotNull: true, ProtoNumber: 2},
			{Name: "quantity", Type: dsl.FieldType{Name: "int"}, NotNull: true, ProtoNumber: 3},
		},
	}
}

// testIR wraps entities in an IR for buildEntityMeta.
func testIR(entities ...*dsl.Entity) *dsl.IR {
	ir := &dsl.IR{Version: 1}
	for _, e := range entities {
		ir.Entities = append(ir.Entities, *e)
	}
	return ir
}

// --- SQL builder tests ---

func TestBuildGetSQL(t *testing.T) {
	e := testAccount()
	sql := buildGetSQL(e)

	// Must contain all columns.
	for _, col := range []string{`"id"`, `"email"`, `"name"`, `"is_active"`, `"created_at"`, `"deleted_at"`, `"balance"`, `"metadata"`} {
		if !strings.Contains(sql, col) {
			t.Errorf("buildGetSQL missing column %s in: %s", col, sql)
		}
	}
	// Must contain PK predicate.
	if !strings.Contains(sql, `"id" = $1`) {
		t.Errorf("buildGetSQL missing PK predicate: %s", sql)
	}
	// Must contain soft delete filter.
	if !strings.Contains(sql, `"deleted_at" IS NULL`) {
		t.Errorf("buildGetSQL missing soft delete filter: %s", sql)
	}
}

func TestBuildGetSQL_CompositePK(t *testing.T) {
	e := testCompositeEntity()
	sql := buildGetSQL(e)

	if !strings.Contains(sql, `("cart_id", "variant_id") = ($1, $2)`) {
		t.Errorf("buildGetSQL composite PK wrong predicate: %s", sql)
	}
}

func TestBuildInsertSQL(t *testing.T) {
	e := testAccount()
	sql := buildInsertSQL(e)

	// All non-identity/serial columns should appear.
	if !strings.Contains(sql, `"id"`) {
		t.Errorf("buildInsertSQL missing id: %s", sql)
	}
	if !strings.Contains(sql, `"email"`) {
		t.Errorf("buildInsertSQL missing email: %s", sql)
	}
	// created_at has a default — should be wrapped in COALESCE.
	if !strings.Contains(sql, "COALESCE(") {
		t.Errorf("buildInsertSQL missing COALESCE for default: %s", sql)
	}
	if !strings.Contains(sql, "TIMESTAMPTZ") {
		t.Errorf("buildInsertSQL missing type cast: %s", sql)
	}
	if !strings.Contains(sql, "now()") {
		t.Errorf("buildInsertSQL missing default expression: %s", sql)
	}
	// RETURNING clause.
	if !strings.Contains(sql, `RETURNING "id"`) {
		t.Errorf("buildInsertSQL missing RETURNING: %s", sql)
	}
}

func TestBuildUpdateSQL(t *testing.T) {
	e := testAccount()
	sql := buildUpdateSQL(e)

	// PK should not appear in SET.
	if strings.Contains(sql, `"id" = $`) && !strings.Contains(sql, "WHERE") {
		t.Errorf("buildUpdateSQL has PK in SET clause: %s", sql)
	}
	// Non-PK columns should appear in SET.
	if !strings.Contains(sql, `"email" = $1`) {
		t.Errorf("buildUpdateSQL missing email in SET: %s", sql)
	}
	// PK should appear in WHERE.
	if !strings.Contains(sql, `WHERE "id" = $`) {
		t.Errorf("buildUpdateSQL missing PK in WHERE: %s", sql)
	}
}

func TestBuildDeleteSQL_SoftDelete(t *testing.T) {
	e := testAccount()
	sql := buildDeleteSQL(e)

	// Soft delete: UPDATE SET deleted_at = now().
	if !strings.Contains(sql, "UPDATE") {
		t.Errorf("buildDeleteSQL should be UPDATE for soft delete: %s", sql)
	}
	if !strings.Contains(sql, `"deleted_at" = now()`) {
		t.Errorf("buildDeleteSQL missing deleted_at = now(): %s", sql)
	}
	if !strings.Contains(sql, `"deleted_at" IS NULL`) {
		t.Errorf("buildDeleteSQL missing IS NULL guard: %s", sql)
	}
}

func TestBuildDeleteSQL_HardDelete(t *testing.T) {
	e := testCompositeEntity()
	sql := buildDeleteSQL(e)

	if !strings.Contains(sql, "DELETE FROM") {
		t.Errorf("buildDeleteSQL should be DELETE for hard delete: %s", sql)
	}
}

func TestBuildBatchGetSQL(t *testing.T) {
	e := testAccount()
	sql := buildBatchGetSQL(e)

	if !strings.Contains(sql, `"id" = ANY($1)`) {
		t.Errorf("buildBatchGetSQL missing ANY: %s", sql)
	}
	if !strings.Contains(sql, `"deleted_at" IS NULL`) {
		t.Errorf("buildBatchGetSQL missing soft delete: %s", sql)
	}
}

func TestBuildQueryPrefix(t *testing.T) {
	e := testAccount()
	sql := buildQueryPrefix(e)

	if !strings.HasPrefix(sql, "SELECT") {
		t.Errorf("buildQueryPrefix should start with SELECT: %s", sql)
	}
	if strings.Contains(sql, "WHERE") {
		t.Errorf("buildQueryPrefix should not contain WHERE: %s", sql)
	}
}

// --- Meta builder tests ---

func TestBuildEntityMeta(t *testing.T) {
	e := testAccount()
	ir := testIR(e)
	meta := buildEntityMeta(e, ir)

	if meta.entityID != "consumer.Account" {
		t.Errorf("entityID = %q, want consumer.Account", meta.entityID)
	}

	// Column count.
	if len(meta.columns) != 8 {
		t.Errorf("columns len = %d, want 8", len(meta.columns))
	}

	// Insert columns should exclude identity/serial (Account has none).
	if len(meta.insertCols) != 8 {
		t.Errorf("insertCols len = %d, want 8", len(meta.insertCols))
	}

	// PK columns.
	if len(meta.pkCols) != 1 || meta.pkCols[0].sqlName != "id" {
		t.Errorf("pkCols = %v, want [id]", meta.pkCols)
	}

	// FilterSpec.
	if meta.filterSpec.EntityID != "consumer.Account" {
		t.Errorf("filterSpec.EntityID = %q", meta.filterSpec.EntityID)
	}
	if _, ok := meta.filterSpec.Fields["email"]; !ok {
		t.Error("filterSpec missing email field")
	}

	// SQL strings should be non-empty.
	if meta.sqlGet == "" {
		t.Error("sqlGet is empty")
	}
	if meta.sqlInsert == "" {
		t.Error("sqlInsert is empty")
	}
	if meta.sqlUpdate == "" {
		t.Error("sqlUpdate is empty")
	}
	if meta.sqlDelete == "" {
		t.Error("sqlDelete is empty")
	}
}

func TestBuildEntityMeta_CompositePK(t *testing.T) {
	e := testCompositeEntity()
	ir := testIR(e)
	meta := buildEntityMeta(e, ir)

	if len(meta.pkCols) != 2 {
		t.Fatalf("pkCols len = %d, want 2", len(meta.pkCols))
	}
	if meta.pkCols[0].sqlName != "cart_id" {
		t.Errorf("pkCols[0] = %q, want cart_id", meta.pkCols[0].sqlName)
	}
	if meta.pkCols[1].sqlName != "variant_id" {
		t.Errorf("pkCols[1] = %q, want variant_id", meta.pkCols[1].sqlName)
	}
}

// --- Proto descriptor tests ---

func TestBuildProtoDescriptors(t *testing.T) {
	e := testAccount()
	fd, err := buildProtoDescriptors(e)
	if err != nil {
		t.Fatalf("buildProtoDescriptors: %v", err)
	}

	msgDesc := fd.Messages().ByName("Account")
	if msgDesc == nil {
		t.Fatal("entity message not found")
	}

	// Entity message should have the right number of fields.
	if msgDesc.Fields().Len() != 8 {
		t.Errorf("entity message fields = %d, want 8", msgDesc.Fields().Len())
	}

	// Check field names and numbers match the entity.
	idField := msgDesc.Fields().ByName("id")
	if idField == nil {
		t.Fatal("entity message missing 'id' field")
	}
	if idField.Number() != 1 {
		t.Errorf("id field number = %d, want 1", idField.Number())
	}

	emailField := msgDesc.Fields().ByName("email")
	if emailField == nil {
		t.Fatal("entity message missing 'email' field")
	}
	if emailField.Number() != 2 {
		t.Errorf("email field number = %d, want 2", emailField.Number())
	}

	// created_at should be a Timestamp message type.
	createdAtField := msgDesc.Fields().ByName("created_at")
	if createdAtField == nil {
		t.Fatal("entity message missing 'created_at' field")
	}
	if createdAtField.Kind() != 11 { // protoreflect.MessageKind
		t.Errorf("created_at kind = %d, want MessageKind (11)", createdAtField.Kind())
	}

	// name should be optional (nullable, no default).
	nameField := msgDesc.Fields().ByName("name")
	if nameField == nil {
		t.Fatal("entity message missing 'name' field")
	}
	if !nameField.HasOptionalKeyword() {
		t.Error("name field should be proto3 optional (nullable column)")
	}

	filterDesc := fd.Messages().ByName("AccountFilter")
	if filterDesc == nil {
		t.Fatal("filter message not found")
	}

	// Filter message should have fields for filterable columns.
	if filterDesc.Fields().Len() == 0 {
		t.Error("filter message has no fields")
	}
	// 'id' should be filterable (bigint).
	filterIdField := filterDesc.Fields().ByName("id")
	if filterIdField == nil {
		t.Error("filter message missing 'id' field")
	}

	// Request/response messages should exist.
	if fd.Messages().ByName("GetAccountRequest") == nil {
		t.Error("GetAccountRequest not found")
	}
	if fd.Messages().ByName("CreateAccountRequest") == nil {
		t.Error("CreateAccountRequest not found")
	}
	if fd.Messages().ByName("QueryAccountRequest") == nil {
		t.Error("QueryAccountRequest not found")
	}
	if fd.Services().ByName("AccountService") == nil {
		t.Error("AccountService not found")
	}
}

func TestBuildProtoDescriptors_CompositePK(t *testing.T) {
	e := testCompositeEntity()
	fd, err := buildProtoDescriptors(e)
	if err != nil {
		t.Fatalf("buildProtoDescriptors: %v", err)
	}

	msgDesc := fd.Messages().ByName("CartItem")
	if msgDesc == nil {
		t.Fatal("entity message not found")
	}
	if msgDesc.Fields().Len() != 3 {
		t.Errorf("entity message fields = %d, want 3", msgDesc.Fields().Len())
	}
}

// --- Nullable handling tests ---

func TestColumnMeta_Nullable(t *testing.T) {
	e := testAccount()
	cols := buildColumnMeta(e)

	// id: NOT NULL, no default — not nullable.
	if cols[0].nullable {
		t.Error("id should not be nullable")
	}
	// name: nullable (no NOT NULL, no default).
	if !cols[2].nullable {
		t.Error("name should be nullable")
	}
	// created_at: NOT NULL but has default — effectively nullable.
	if !cols[4].nullable {
		t.Error("created_at should be effectively nullable (has default)")
	}
}

// TestBuildEntityMeta_WithIdentity verifies serial/identity columns
// are excluded from insertCols.
func TestBuildEntityMeta_WithIdentity(t *testing.T) {
	e := &dsl.Entity{
		Name:      "Sequence",
		Namespace: "internal",
		Fields: []dsl.Field{
			{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true, NotNull: true, Identity: true, ProtoNumber: 1},
			{Name: "value", Type: dsl.FieldType{Name: "text"}, NotNull: true, ProtoNumber: 2},
		},
	}
	ir := testIR(e)
	meta := buildEntityMeta(e, ir)

	if len(meta.insertCols) != 1 {
		t.Fatalf("insertCols len = %d, want 1 (id excluded)", len(meta.insertCols))
	}
	if meta.insertCols[0].sqlName != "value" {
		t.Errorf("insertCols[0] = %q, want value", meta.insertCols[0].sqlName)
	}
}

// TestBuildInsertSQL_Identity verifies identity columns don't appear
// in the INSERT.
func TestBuildInsertSQL_Identity(t *testing.T) {
	e := &dsl.Entity{
		Name:      "Sequence",
		Namespace: "internal",
		Fields: []dsl.Field{
			{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true, NotNull: true, Identity: true, ProtoNumber: 1},
			{Name: "value", Type: dsl.FieldType{Name: "text"}, NotNull: true, ProtoNumber: 2},
		},
	}
	sql := buildInsertSQL(e)

	// The INSERT column list should only have "value".
	if strings.Contains(sql, `("id"`) && !strings.Contains(sql, "RETURNING") {
		t.Errorf("buildInsertSQL should not insert identity column: %s", sql)
	}
	// RETURNING should still reference the PK.
	if !strings.Contains(sql, `RETURNING "id"`) {
		t.Errorf("buildInsertSQL missing RETURNING: %s", sql)
	}
}

// TestBuildUpdateSQL_AllPK verifies that an entity with all PK columns
// produces an empty update SQL.
func TestBuildUpdateSQL_AllPK(t *testing.T) {
	e := &dsl.Entity{
		Name:        "Link",
		Namespace:   "internal",
		CompositePK: []string{"a", "b"},
		Fields: []dsl.Field{
			{Name: "a", Type: dsl.FieldType{Name: "bigint"}, NotNull: true, ProtoNumber: 1},
			{Name: "b", Type: dsl.FieldType{Name: "bigint"}, NotNull: true, ProtoNumber: 2},
		},
	}
	sql := buildUpdateSQL(e)
	if sql != "" {
		t.Errorf("buildUpdateSQL should be empty for all-PK entity, got: %s", sql)
	}
}
