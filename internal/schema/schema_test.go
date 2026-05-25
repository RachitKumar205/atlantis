package schema

import (
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

func TestQuoteIdent(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"id", `"id"`},
		{"consumer_account", `"consumer_account"`},
		{`has"quote`, `"has""quote"`},
	}
	for _, tt := range tests {
		got := QuoteIdent(tt.in)
		if got != tt.want {
			t.Errorf("QuoteIdent(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSnakeCase(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"Account", "account"},
		{"ProductVariant", "product_variant"},
		{"APIKey", "api_key"},
		{"CartItem", "cart_item"},
	}
	for _, tt := range tests {
		got := SnakeCase(tt.in)
		if got != tt.want {
			t.Errorf("SnakeCase(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSQLType(t *testing.T) {
	tests := []struct {
		in   dsl.FieldType
		want string
	}{
		{dsl.FieldType{Name: "bigint"}, "BIGINT"},
		{dsl.FieldType{Name: "varchar", Len: 255}, "VARCHAR(255)"},
		{dsl.FieldType{Name: "vector", VecDim: 768}, "vector(768)"},
		{dsl.FieldType{Name: "numeric", HasNumP: true, NumP: 10, NumS: 2}, "NUMERIC(10, 2)"},
		{dsl.FieldType{Name: "text", Array: true, Elem: &dsl.FieldType{Name: "text"}}, "TEXT[]"},
	}
	for _, tt := range tests {
		got := SQLType(tt.in)
		if got != tt.want {
			t.Errorf("SQLType(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func testEntity() *dsl.Entity {
	return &dsl.Entity{
		Name:      "Account",
		Namespace: "consumer",
		Fields: []dsl.Field{
			{Name: "id", Type: dsl.FieldType{Name: "bigint"}, Primary: true, NotNull: true},
			{Name: "email", Type: dsl.FieldType{Name: "citext"}, NotNull: true},
			{Name: "name", Type: dsl.FieldType{Name: "varchar", Len: 255}},
			{Name: "seq", Type: dsl.FieldType{Name: "bigint"}, Identity: true},
		},
	}
}

func TestQualifiedTable(t *testing.T) {
	e := testEntity()
	got := QualifiedTable(e)
	want := `"atlantis"."consumer_account"`
	if got != want {
		t.Errorf("QualifiedTable = %q, want %q", got, want)
	}

	// With table override.
	e2 := *e
	e2.TableName = "public.accounts"
	got2 := QualifiedTable(&e2)
	want2 := `"public"."accounts"`
	if got2 != want2 {
		t.Errorf("QualifiedTable (override) = %q, want %q", got2, want2)
	}
}

func TestFieldColumns(t *testing.T) {
	e := testEntity()
	cols := FieldColumns(e)
	want := []string{"id", "email", "name", "seq"}
	if len(cols) != len(want) {
		t.Fatalf("FieldColumns len = %d, want %d", len(cols), len(want))
	}
	for i, c := range cols {
		if c != want[i] {
			t.Errorf("FieldColumns[%d] = %q, want %q", i, c, want[i])
		}
	}
}

func TestInsertColumns(t *testing.T) {
	e := testEntity()
	cols := InsertColumns(e)
	// Identity column 'seq' should be excluded.
	want := []string{"id", "email", "name"}
	if len(cols) != len(want) {
		t.Fatalf("InsertColumns len = %d, want %d", len(cols), len(want))
	}
	for i, c := range cols {
		if c != want[i] {
			t.Errorf("InsertColumns[%d] = %q, want %q", i, c, want[i])
		}
	}
}

func TestPKColumns(t *testing.T) {
	e := testEntity()
	pk := PKColumns(e)
	if len(pk) != 1 || pk[0].Name != "id" {
		t.Fatalf("PKColumns = %v, want [id]", pk)
	}

	// Composite PK.
	e2 := &dsl.Entity{
		Name:        "CartItem",
		Namespace:   "consumer",
		CompositePK: []string{"cart_id", "variant_id"},
		Fields: []dsl.Field{
			{Name: "cart_id", Type: dsl.FieldType{Name: "bigint"}, NotNull: true},
			{Name: "variant_id", Type: dsl.FieldType{Name: "bigint"}, NotNull: true},
			{Name: "quantity", Type: dsl.FieldType{Name: "int"}, NotNull: true},
		},
	}
	pk2 := PKColumns(e2)
	if len(pk2) != 2 || pk2[0].Name != "cart_id" || pk2[1].Name != "variant_id" {
		t.Fatalf("PKColumns composite = %v, want [cart_id, variant_id]", pk2)
	}
}

func TestDefaultExpr(t *testing.T) {
	tests := []struct {
		in   dsl.Default
		want string
	}{
		{dsl.Default{Kind: dsl.DefaultIRNow}, "now()"},
		{dsl.Default{Kind: dsl.DefaultIRString, Str: "hello"}, "'hello'"},
		{dsl.Default{Kind: dsl.DefaultIRInt, Int: 42}, "42"},
		{dsl.Default{Kind: dsl.DefaultIRBool, Bool: true}, "TRUE"},
		{dsl.Default{Kind: dsl.DefaultIRBool, Bool: false}, "FALSE"},
	}
	for _, tt := range tests {
		got := DefaultExpr(tt.in)
		if got != tt.want {
			t.Errorf("DefaultExpr(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestIsEffectivelyNullable(t *testing.T) {
	notNull := &dsl.Field{Name: "id", NotNull: true}
	nullable := &dsl.Field{Name: "name", NotNull: false}
	withDefault := &dsl.Field{Name: "created_at", NotNull: true, Default: &dsl.Default{Kind: dsl.DefaultIRNow}}

	if IsEffectivelyNullable(notNull) {
		t.Error("NOT NULL field without default should not be nullable")
	}
	if !IsEffectivelyNullable(nullable) {
		t.Error("nullable field should be nullable")
	}
	if !IsEffectivelyNullable(withDefault) {
		t.Error("NOT NULL field with default should be effectively nullable")
	}
}
