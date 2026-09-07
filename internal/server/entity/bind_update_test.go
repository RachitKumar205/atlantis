package entity

import (
	"regexp"
	"strconv"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/rachitkumar205/atlantis/internal/cache/invalidate"
)

// wideSchema crosses every presence kind bindForUpdate distinguishes: an
// implicit scalar, an optional scalar, a defaulted NOT NULL column, a
// message-typed NOT NULL column, an array and a vector.
const wideSchema = `
entity Wide in bw {
  id         bigint primary serial
  title      text not null
  body       text
  created_at timestamptz not null default now()
  seen_at    timestamptz not null
  tags       []text
  embedding  vector(3)
}
`

func wideMeta(t *testing.T) *entityMeta {
	t.Helper()
	srv := NewServer(&notFoundPool{}, noopCache{}, invalidate.NewOutbox(), nil, nil)
	if err := srv.Load(routeIR(t, wideSchema)); err != nil {
		t.Fatal(err)
	}
	return srv.snapshot.Load().entities["bw.Wide"]
}

var placeholder = regexp.MustCompile(`\$(\d+)`)

func highestPlaceholder(sql string) int {
	n := 0
	for _, m := range placeholder.FindAllStringSubmatch(sql, -1) {
		if v, _ := strconv.Atoi(m[1]); v > n {
			n = v
		}
	}
	return n
}

// TestBindForUpdateMatchesThePlaceholders pins the argument count of every
// write statement to the placeholders it renders, so the SET list and the
// bind layout cannot drift apart.
func TestBindForUpdateMatchesThePlaceholders(t *testing.T) {
	meta := wideMeta(t)
	msg := dynamicpb.NewMessage(meta.msgDesc)
	if got, want := len(bindForUpdate(meta, msg, nil)), highestPlaceholder(meta.sqlUpdate); got != want {
		t.Errorf("bindForUpdate binds %d arguments, sqlUpdate has %d placeholders:\n%s", got, want, meta.sqlUpdate)
	}
	if got, want := len(bindForInsert(meta, msg)), highestPlaceholder(meta.sqlInsert); got != want {
		t.Errorf("bindForInsert binds %d arguments, sqlInsert has %d placeholders:\n%s", got, want, meta.sqlInsert)
	}
}

// TestBindForUpdatePresenceByKind pins the write flag bound for each field
// kind, with and without a mask.
func TestBindForUpdatePresenceByKind(t *testing.T) {
	meta := wideMeta(t)
	fields := meta.msgDesc.Fields()

	flags := func(msg *dynamicpb.Message, mask ...string) map[string]bool {
		args := bindForUpdate(meta, msg, mask)
		out := make(map[string]bool, len(meta.updateCols))
		for i, cm := range meta.updateCols {
			out[cm.field.Name] = args[2*i].(bool)
		}
		return out
	}

	empty := dynamicpb.NewMessage(meta.msgDesc)
	got := flags(empty)
	want := map[string]bool{"title": true, "body": false, "created_at": false, "seen_at": false, "tags": false, "embedding": false}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("empty message, no mask: %s written=%v, want %v", name, got[name], w)
		}
	}

	set := dynamicpb.NewMessage(meta.msgDesc)
	set.Set(fields.ByName("body"), protoreflect.ValueOfString(""))
	set.Mutable(fields.ByName("tags")).List().Append(protoreflect.ValueOfString("a"))
	set.Mutable(fields.ByName("embedding")).List().Append(protoreflect.ValueOfFloat32(1))
	got = flags(set)
	for _, name := range []string{"title", "body", "tags", "embedding"} {
		if !got[name] {
			t.Errorf("set message, no mask: %s not written", name)
		}
	}

	// A mask writes what it names and nothing else, set or not.
	got = flags(set, "seen_at")
	for name, w := range map[string]bool{"seen_at": true, "title": false, "body": false, "tags": false, "embedding": false} {
		if got[name] != w {
			t.Errorf("mask [seen_at]: %s written=%v, want %v", name, got[name], w)
		}
	}
}
