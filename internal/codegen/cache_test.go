package codegen

import (
	"testing"
)

func TestEmitGoCacheKeys_BodyAndPointerKey(t *testing.T) {
	ir := lower(t, `entity SavedOutfit in consumer { id bigint primary  consumer_id bigint not null }`)
	files, err := EmitGoCacheKeys(ir)
	if err != nil {
		t.Fatalf("EmitGoCacheKeys: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %d", len(files))
	}
	c := findFile(t, files, "gen/go/keys/consumer/saved_outfit_keys.go")
	parseAsGo(t, c)

	// Body & pointer key functions both present. The CompositeID call is
	// load-bearing: it length-prefixes the PK value before the key is
	// built, matching the encoding the server hot path uses when writing
	// cache entries. fmt.Sprint(id) would silently produce a different
	// key for the same id, so any external caller computing a body key
	// from the keys package would miss the cache; the design note in
	// internal/codegen/cache.go records why this was a real bug.
	assertContains(t, c, "func SavedOutfitBodyKey(id int64, version int64) string")
	assertContains(t, c, `runtime.CacheKey("consumer.SavedOutfit", runtime.CompositeID(id), version)`)
	assertContains(t, c, "func SavedOutfitPointerKey(id int64) string")
	assertContains(t, c, `runtime.PointerKey("consumer.SavedOutfit", runtime.CompositeID(id))`)
}

// TestEmitGoCacheKeys_CompositePK_EmitsRealKeys pins the shape that
// composite-PK entities expose to external callers: one parameter per PK
// column (DSL declaration order) routed through runtime.CompositeID, so
// the resulting body/pointer key is byte-for-byte what the server's
// outbox path produced when writing the cache entry. Earlier revisions
// emitted only a header-only deferral stub here; that left composite-PK
// rows uncacheable by external callers.
func TestEmitGoCacheKeys_CompositePK_EmitsRealKeys(t *testing.T) {
	ir := lower(t, `entity CartItem in consumer {
  cart_id    bigint not null
  variant_id bigint not null
  primary by cart_id, variant_id
}`)
	files, err := EmitGoCacheKeys(ir)
	if err != nil {
		t.Fatalf("EmitGoCacheKeys: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %d", len(files))
	}
	c := findFile(t, files, "gen/go/keys/consumer/cart_item_keys.go")
	parseAsGo(t, c)

	assertContains(t, c, "func CartItemBodyKey(cartId int64, variantId int64, version int64) string")
	assertContains(t, c, `runtime.CacheKey("consumer.CartItem", runtime.CompositeID(cartId, variantId), version)`)
	assertContains(t, c, "func CartItemPointerKey(cartId int64, variantId int64) string")
	assertContains(t, c, `runtime.PointerKey("consumer.CartItem", runtime.CompositeID(cartId, variantId))`)
	// The deferral stub is gone.
	assertNotContains(t, c, "deferred to v0.2")
}

func TestEmitGoCacheKeys_TagExpansion(t *testing.T) {
	ir := lower(t, `
entity SavedOutfit in consumer {
  id          bigint primary
  consumer_id bigint not null
  cache { read_through ttl=10m tag="consumer:{consumer_id}" }
}
`)
	files, _ := EmitGoCacheKeys(ir)
	c := findFile(t, files, "gen/go/keys/consumer/saved_outfit_keys.go")
	parseAsGo(t, c)

	// Tag function: one parameter (consumer_id), body concatenates the literal + the placeholder.
	assertContains(t, c, "func SavedOutfitTagKey(consumerId int64) string")
	assertContains(t, c, `"consumer:" + fmt.Sprint(consumerId)`)
}

func TestEmitGoCacheKeys_NoTagWhenAbsent(t *testing.T) {
	ir := lower(t, `entity A in x { id bigint primary  cache { read_through ttl=10m } }`)
	files, _ := EmitGoCacheKeys(ir)
	c := findFile(t, files, "gen/go/keys/x/a_keys.go")
	assertNotContains(t, c, "TagKey")
}

func TestEmitGoCacheKeys_IndexKeyForBtree(t *testing.T) {
	ir := lower(t, `
entity Account in consumer {
  id          bigint primary
  consumer_id bigint not null
  created_at  timestamptz default now()
  index by consumer_id, created_at desc
}
`)
	files, _ := EmitGoCacheKeys(ir)
	c := findFile(t, files, "gen/go/keys/consumer/account_keys.go")
	parseAsGo(t, c)

	// Function name reflects the composite of index fields.
	assertContains(t, c, "func AccountByConsumerIdCreatedAtKey(consumerId int64, createdAt time.Time) string")
	// Parts hashed and prefixed with `atl:v1:{id}:idx:{name}:`
	assertContains(t, c, `"atl:v1:consumer.Account:idx:consumer_id_created_at_desc:"`)
	// Args are encoded via the runtime helper, not bare fmt.Sprint, so
	// values containing the ":" separator can't collide.
	assertContains(t, c, "runtime.EncodeKeyArg(consumerId)")
	assertContains(t, c, "runtime.EncodeKeyArg(createdAt)")
}

func TestEmitGoCacheKeys_IndexKeyForPartial(t *testing.T) {
	ir := lower(t, `
entity A in x {
  id         bigint primary
  consumer   bigint
  deleted_at timestamptz
  index partial by consumer where deleted_at is null
}
`)
	files, _ := EmitGoCacheKeys(ir)
	c := findFile(t, files, "gen/go/keys/x/a_keys.go")
	parseAsGo(t, c)
	// Partial indexes get a "Partial" infix plus a predicate-derived suffix
	// in the function name. The suffix disambiguates two partials on the
	// same field set with different WHERE clauses.
	assertContains(t, c, "func APartialByConsumerWhereDeletedAtIsNullKey(consumer int64) string")
	assertContains(t, c, `"atl:v1:x.A:idx:consumer_w_deleted_at_is_null:"`)
}

// Two partial indexes covering the same field set but filtering by different
// literal values must produce distinct cache-key functions; otherwise the
// emitted Go fails to compile (function-name redeclared). OutfitInteraction's
// `action = "purchased"` and `action = "added_to_cart"` partials are the
// real-world case that surfaces this.
func TestEmitGoCacheKeys_PartialIndexPredicateDisambiguatesFnName(t *testing.T) {
	ir := lower(t, `
entity A in x {
  id      bigint primary
  user_id bigint
  action  varchar(20) not null
  index partial by user_id where action = "purchased"
  index partial by user_id where action = "added_to_cart"
}
`)
	files, _ := EmitGoCacheKeys(ir)
	c := findFile(t, files, "gen/go/keys/x/a_keys.go")
	parseAsGo(t, c)
	assertContains(t, c, "func APartialByUserIdWhereActionEqPurchasedKey(")
	assertContains(t, c, "func APartialByUserIdWhereActionEqAddedToCartKey(")
}

func TestEmitGoCacheKeys_GoKeywordFieldNameSanitized(t *testing.T) {
	// `type` is both a common DSL field name (e.g. Promotion.type) and a Go
	// reserved word — emitting it verbatim as a parameter name produces
	// invalid Go. goParamName suffixes "Val" to defuse the collision.
	ir := lower(t, `
entity Promotion in vendor {
  id   bigint primary
  type text not null
  index by type
}
`)
	files, _ := EmitGoCacheKeys(ir)
	c := findFile(t, files, "gen/go/keys/vendorpkg/promotion_keys.go")
	parseAsGo(t, c)
	assertContains(t, c, "func PromotionByTypeKey(typeVal string) string")
	assertContains(t, c, "runtime.EncodeKeyArg(typeVal)")
	assertNotContains(t, c, "(type string)")
}

func TestEmitGoCacheKeys_HNSWAndGINSkipped(t *testing.T) {
	ir := lower(t, `
entity P in x {
  id   bigint primary
  vec  vector(8)
  meta jsonb
  index hnsw on vec ops cosine
  index gin on meta
}
`)
	files, _ := EmitGoCacheKeys(ir)
	c := findFile(t, files, "gen/go/keys/x/p_keys.go")
	// HNSW and GIN indexes get no cache key function per PLAN §B.4.
	assertNotContains(t, c, "Hnsw")
	assertNotContains(t, c, "ByVec")
	assertNotContains(t, c, "ByMeta")
}

func TestEmitGoCacheKeys_ImportsRuntime(t *testing.T) {
	ir := lower(t, `entity A in x { id bigint primary }`)
	files, _ := EmitGoCacheKeys(ir)
	c := findFile(t, files, "gen/go/keys/x/a_keys.go")
	assertContains(t, c, `"github.com/rachitkumar205/atlantis/internal/runtime"`)
}

func TestEmitGoCacheKeys_DeterministicAcrossRuns(t *testing.T) {
	src := `
entity A in x {
  id          bigint primary
  consumer_id bigint not null
  created_at  timestamptz
  index by consumer_id, created_at desc
  cache { read_through ttl=10m tag="x:{consumer_id}:{id}" }
}
`
	ir1 := lower(t, src)
	files1, _ := EmitGoCacheKeys(ir1)
	ir2 := lower(t, src)
	files2, _ := EmitGoCacheKeys(ir2)
	if len(files1) != len(files2) {
		t.Fatalf("file count mismatch")
	}
	for i := range files1 {
		if files1[i].Path != files2[i].Path {
			t.Errorf("path drift at %d", i)
		}
		if files1[i].Content != files2[i].Content {
			t.Errorf("content drift at %d", i)
		}
	}
}

func TestEmitGoCacheKeys_RepeatedPlaceholdersInTag(t *testing.T) {
	// {x} appears twice — only one parameter should appear in the signature.
	ir := lower(t, `entity A in x { id bigint primary  cache { read_through ttl=10m tag="{id}-{id}" } }`)
	files, _ := EmitGoCacheKeys(ir)
	c := findFile(t, files, "gen/go/keys/x/a_keys.go")
	parseAsGo(t, c)
	assertContains(t, c, "func ATagKey(id int64) string")
	// Body uses the parameter twice.
	assertContains(t, c, "fmt.Sprint(id) + ")
}

func TestSplitTagTemplate(t *testing.T) {
	cases := []struct {
		in   string
		want []tagPiece
	}{
		{"", nil},
		{"hi", []tagPiece{{value: "hi"}}},
		{"{x}", []tagPiece{{value: "x", isField: true}}},
		{"a:{x}", []tagPiece{{value: "a:"}, {value: "x", isField: true}}},
		{"{x}:b", []tagPiece{{value: "x", isField: true}, {value: ":b"}}},
		{"{x}-{y}", []tagPiece{
			{value: "x", isField: true},
			{value: "-"},
			{value: "y", isField: true},
		}},
		{"unmatched {", []tagPiece{{value: "unmatched {"}}},
	}
	for _, c := range cases {
		got := splitTagTemplate(c.in)
		if len(got) != len(c.want) {
			t.Errorf("splitTagTemplate(%q) length mismatch: got %d want %d (%v)", c.in, len(got), len(c.want), got)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("splitTagTemplate(%q)[%d] = %v want %v", c.in, i, got[i], c.want[i])
			}
		}
	}
}
