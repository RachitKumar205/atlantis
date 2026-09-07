package entity

import (
	"context"
	"os"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/rachitkumar205/atlantis/internal/cache/invalidate"
	"github.com/rachitkumar205/atlantis/internal/codegen"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/storage/pg"
)

// TestUpdateWritesOnlyWhatWasSet runs the dispatcher's Update against
// Postgres and pins the presence rule end to end: an unset optional field
// keeps its stored value, a set one is written, a field named in update_mask
// is written whether or not it was set, and a field with implicit presence
// is written from its value.
//
// The row has `created_at timestamptz not null default now()`, and the
// first update carries the title and nothing else.
func TestUpdateWritesOnlyWhatWasSet(t *testing.T) {
	dsn := os.Getenv("ATLANTIS_TEST_PG")
	if dsn == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the dispatcher against Postgres")
	}
	ctx := context.Background()
	pool, err := pg.New(ctx, pg.DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS atlantis.updp_note`)
	if _, err := pool.Exec(ctx, `CREATE TABLE atlantis.updp_note (
		id         bigserial PRIMARY KEY,
		title      varchar(200) NOT NULL,
		body       text,
		created_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS atlantis.updp_note`) })

	f, err := dsl.Parse("updp.atl", []byte(`
entity Note in updp {
  id         bigint primary serial
  title      varchar(200) not null
  body       text
  created_at timestamptz not null default now()
}
`))
	if err != nil {
		t.Fatal(err)
	}
	ir, err := dsl.Lower([]*dsl.File{f})
	if err != nil {
		t.Fatal(err)
	}
	codegen.AssignProtoNumbers(nil, ir)
	srv := NewServer(pool, noopCache{}, invalidate.NewOutbox(), nil, nil)
	if err := srv.Reload(ir, "updp"); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	meta := srv.snapshot.Load().entities["updp.Note"]
	if meta == nil {
		t.Fatal("updp.Note is not in the snapshot")
	}
	fields := meta.msgDesc.Fields()

	// Create with only the title; created_at comes from the default.
	create := dynamicpb.NewMessage(meta.createRequestDesc)
	{
		e := dynamicpb.NewMessage(meta.msgDesc)
		e.Set(fields.ByName("title"), protoreflect.ValueOfString("first"))
		create.Set(meta.createRequestDesc.Fields().ByName("entity"), protoreflect.ValueOfMessage(e))
	}
	created, err := srv.handleCreate(ctx, meta, decodeFrom(create))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	row := created.(*dynamicpb.Message).Get(meta.createResponseDesc.Fields().ByName("entity")).Message()
	id := row.Get(fields.ByName("id")).Int()
	createdAt := row.Get(fields.ByName("created_at")).Message().Interface()

	read := func() (title, body string, hasBody bool, sameCreatedAt bool) {
		t.Helper()
		get := dynamicpb.NewMessage(meta.getRequestDesc)
		get.Set(meta.getRequestDesc.Fields().ByNumber(1), protoreflect.ValueOfInt64(id))
		resp, err := srv.handleGet(ctx, meta, decodeFrom(get))
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		r := resp.(*dynamicpb.Message).Get(meta.getResponseDesc.Fields().ByName("entity")).Message()
		var ca string
		if m := r.Get(fields.ByName("created_at")).Message(); m.IsValid() {
			ca = m.Interface().(interface{ String() string }).String()
		}
		return r.Get(fields.ByName("title")).String(),
			r.Get(fields.ByName("body")).String(),
			r.Has(fields.ByName("body")),
			ca == createdAt.(interface{ String() string }).String()
	}

	update := func(mutate func(e *dynamicpb.Message), mask ...string) error {
		t.Helper()
		req := dynamicpb.NewMessage(meta.updateRequestDesc)
		e := dynamicpb.NewMessage(meta.msgDesc)
		e.Set(fields.ByName("id"), protoreflect.ValueOfInt64(id))
		mutate(e)
		req.Set(meta.updateRequestDesc.Fields().ByName("entity"), protoreflect.ValueOfMessage(e))
		if len(mask) > 0 {
			mfd := meta.updateRequestDesc.Fields().ByName("update_mask")
			m := req.NewField(mfd).Message()
			paths := m.Mutable(m.Descriptor().Fields().ByName("paths")).List()
			for _, p := range mask {
				paths.Append(protoreflect.ValueOfString(p))
			}
			req.Set(mfd, protoreflect.ValueOfMessage(m))
		}
		_, err := srv.handleUpdate(ctx, meta, decodeFrom(req))
		return err
	}

	// 1. Title only. created_at was not sent and must survive.
	if err := update(func(e *dynamicpb.Message) {
		e.Set(fields.ByName("title"), protoreflect.ValueOfString("second"))
	}); err != nil {
		t.Fatalf("update with created_at unset: %v", err)
	}
	if title, _, hasBody, same := read(); title != "second" || hasBody || !same {
		t.Fatalf("after title-only update: title=%q hasBody=%v createdAtKept=%v", title, hasBody, same)
	}

	// 2. A set optional field is written.
	if err := update(func(e *dynamicpb.Message) {
		e.Set(fields.ByName("title"), protoreflect.ValueOfString("second"))
		e.Set(fields.ByName("body"), protoreflect.ValueOfString("some body"))
	}); err != nil {
		t.Fatalf("update setting body: %v", err)
	}
	if _, body, hasBody, _ := read(); !hasBody || body != "some body" {
		t.Fatalf("after setting body: body=%q hasBody=%v", body, hasBody)
	}

	// 3. An unset optional field keeps its value — body survives a request
	//    that did not mention it.
	if err := update(func(e *dynamicpb.Message) {
		e.Set(fields.ByName("title"), protoreflect.ValueOfString("third"))
	}); err != nil {
		t.Fatalf("update leaving body unset: %v", err)
	}
	if title, body, _, _ := read(); title != "third" || body != "some body" {
		t.Fatalf("after leaving body unset: title=%q body=%q", title, body)
	}

	// 4. Naming an unset field in update_mask writes NULL into it: the one
	//    way to clear a nullable column through Update.
	if err := update(func(e *dynamicpb.Message) {
		e.Set(fields.ByName("title"), protoreflect.ValueOfString("third"))
	}, "body"); err != nil {
		t.Fatalf("update with body masked and unset: %v", err)
	}
	if _, body, hasBody, _ := read(); hasBody || body != "" {
		t.Fatalf("after masking body: body=%q hasBody=%v, want NULL", body, hasBody)
	}

	// 5. A mask that names other fields leaves an unnamed set field alone.
	if err := update(func(e *dynamicpb.Message) {
		e.Set(fields.ByName("title"), protoreflect.ValueOfString("fourth"))
		e.Set(fields.ByName("body"), protoreflect.ValueOfString("ignored"))
	}, "title"); err != nil {
		t.Fatalf("update with mask naming title only: %v", err)
	}
	if title, _, hasBody, _ := read(); title != "fourth" || hasBody {
		t.Fatalf("after masking title only: title=%q hasBody=%v", title, hasBody)
	}

	// 6. A field with implicit presence is written from its value: the empty
	//    string is a value, not an absence.
	if err := update(func(e *dynamicpb.Message) {
		e.Set(fields.ByName("title"), protoreflect.ValueOfString(""))
	}); err != nil {
		t.Fatalf("update with empty title: %v", err)
	}
	if title, _, _, _ := read(); title != "" {
		t.Fatalf("after empty title: title=%q, want empty", title)
	}

	// 7. A mask that does not name a field with implicit presence leaves it
	//    alone too: a request carrying only the key and a mask of ["body"]
	//    must not blank the title.
	if err := update(func(e *dynamicpb.Message) {
		e.Set(fields.ByName("title"), protoreflect.ValueOfString("kept"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := update(func(e *dynamicpb.Message) {
		e.Set(fields.ByName("body"), protoreflect.ValueOfString("only body"))
	}, "body"); err != nil {
		t.Fatalf("update with mask [body] and title unset: %v", err)
	}
	if title, body, _, _ := read(); title != "kept" || body != "only body" {
		t.Fatalf("after mask [body] with title unset: title=%q body=%q", title, body)
	}

	// 8. A mask path that names no updatable column is refused.
	err = update(func(e *dynamicpb.Message) {}, "bodyy")
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("mask naming an unknown field: %v, want InvalidArgument", err)
	}
	err = update(func(e *dynamicpb.Message) {}, "id")
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("mask naming the primary key: %v, want InvalidArgument", err)
	}
}

// TestUpdateCaseFormAcrossColumnTypes runs the two-parameter SET form
// against a column of every class the dispatcher binds, so the CASE
// type-resolves for each of them in Postgres: an update carrying only the
// title keeps every other column, and one carrying every column writes it.
func TestUpdateCaseFormAcrossColumnTypes(t *testing.T) {
	dsn := os.Getenv("ATLANTIS_TEST_PG")
	if dsn == "" {
		t.Skip("set ATLANTIS_TEST_PG to exercise the dispatcher against Postgres")
	}
	ctx := context.Background()
	pool, err := pg.New(ctx, pg.DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	_, _ = pool.Exec(ctx, `DROP TABLE IF EXISTS atlantis.updw_row`)
	if _, err := pool.Exec(ctx, `CREATE TABLE atlantis.updw_row (
		id         bigserial PRIMARY KEY,
		title      text NOT NULL,
		n32        integer,
		n64        bigint,
		flag       boolean,
		f32        real,
		f64        double precision,
		amount     numeric(12,2),
		doc        jsonb,
		blob       bytea,
		span       interval,
		seen_at    timestamptz,
		day        date,
		tags       text[],
		embedding  vector(3),
		created_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP TABLE IF EXISTS atlantis.updw_row`) })

	f, err := dsl.Parse("updw.atl", []byte(`
entity Row in updw {
  id         bigint primary serial
  title      text not null
  n32        int
  n64        bigint
  flag       boolean
  f32        real
  f64        double
  amount     numeric(12,2)
  doc        jsonb
  blob       bytea
  span       interval
  seen_at    timestamptz
  day        date
  tags       []text
  embedding  vector(3)
  created_at timestamptz not null default now()
}
`))
	if err != nil {
		t.Fatal(err)
	}
	ir, err := dsl.Lower([]*dsl.File{f})
	if err != nil {
		t.Fatal(err)
	}
	codegen.AssignProtoNumbers(nil, ir)
	srv := NewServer(pool, noopCache{}, invalidate.NewOutbox(), nil, nil)
	if err := srv.Reload(ir, "updw"); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	meta := srv.snapshot.Load().entities["updw.Row"]

	create := dynamicpb.NewMessage(meta.createRequestDesc)
	e := dynamicpb.NewMessage(meta.msgDesc)
	e.Set(meta.msgDesc.Fields().ByName("title"), protoreflect.ValueOfString("first"))
	create.Set(meta.createRequestDesc.Fields().ByName("entity"), protoreflect.ValueOfMessage(e))
	created, err := srv.handleCreate(ctx, meta, decodeFrom(create))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	id := created.(*dynamicpb.Message).Get(meta.createResponseDesc.Fields().ByName("entity")).Message().Get(meta.msgDesc.Fields().ByName("id")).Int()

	// Every nullable column NULL, then an update that names none of them:
	// the CASE must type-resolve with an unset parameter of every class.
	req := dynamicpb.NewMessage(meta.updateRequestDesc)
	e = dynamicpb.NewMessage(meta.msgDesc)
	e.Set(meta.msgDesc.Fields().ByName("id"), protoreflect.ValueOfInt64(id))
	e.Set(meta.msgDesc.Fields().ByName("title"), protoreflect.ValueOfString("second"))
	req.Set(meta.updateRequestDesc.Fields().ByName("entity"), protoreflect.ValueOfMessage(e))
	if _, err := srv.handleUpdate(ctx, meta, decodeFrom(req)); err != nil {
		t.Fatalf("update with every other column unset: %v", err)
	}

	var title string
	var nulls int
	if err := pool.QueryRow(ctx, `SELECT title,
		(n32 IS NULL)::int + (n64 IS NULL)::int + (flag IS NULL)::int + (f32 IS NULL)::int + (f64 IS NULL)::int +
		(amount IS NULL)::int + (doc IS NULL)::int + (blob IS NULL)::int + (span IS NULL)::int + (seen_at IS NULL)::int +
		(day IS NULL)::int + (tags IS NULL)::int + (embedding IS NULL)::int
		FROM atlantis.updw_row WHERE id = $1`, id).Scan(&title, &nulls); err != nil {
		t.Fatal(err)
	}
	if title != "second" || nulls != 13 {
		t.Fatalf("after title-only update: title=%q, %d of 13 optional columns NULL", title, nulls)
	}

	// Now write each of them by SQL, and update title-only again: all 13
	// must keep their values.
	if _, err := pool.Exec(ctx, `UPDATE atlantis.updw_row SET n32 = 1, n64 = 2, flag = true, f32 = 1.5, f64 = 2.5,
		amount = 3.25, doc = '{"a":1}', blob = '\x01', span = '1 day', seen_at = now(), day = current_date,
		tags = '{x,y}', embedding = '[1,2,3]' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	e.Set(meta.msgDesc.Fields().ByName("title"), protoreflect.ValueOfString("third"))
	if _, err := srv.handleUpdate(ctx, meta, decodeFrom(req)); err != nil {
		t.Fatalf("second title-only update: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT title,
		(n32 IS NULL)::int + (n64 IS NULL)::int + (flag IS NULL)::int + (f32 IS NULL)::int + (f64 IS NULL)::int +
		(amount IS NULL)::int + (doc IS NULL)::int + (blob IS NULL)::int + (span IS NULL)::int + (seen_at IS NULL)::int +
		(day IS NULL)::int + (tags IS NULL)::int + (embedding IS NULL)::int
		FROM atlantis.updw_row WHERE id = $1`, id).Scan(&title, &nulls); err != nil {
		t.Fatal(err)
	}
	if title != "third" || nulls != 0 {
		t.Fatalf("after title-only update over populated columns: title=%q, %d columns lost their value", title, nulls)
	}
}

// decodeFrom returns a dec closure that copies msg into the handler's
// request, the way the gRPC codec would.
func decodeFrom(msg *dynamicpb.Message) func(any) error {
	return func(dst any) error {
		d := dst.(*dynamicpb.Message)
		msg.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
			d.Set(fd, v)
			return true
		})
		return nil
	}
}
