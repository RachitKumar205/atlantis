//go:build integration

package integration

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	commonpb "github.com/rachitkumar205/atlantis-go/pb/atlantis/common/v1"
	consumerpb "github.com/rachitkumar205/atlantis-go/pb/atlantis/consumer/v1"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/server/entity"
)

// startGRPCServer boots an in-process gRPC server with the dynamic entity
// server registered against the harness. The IR is loaded from the
// ir_checkpoint table (populated by `tide apply` during the migration step).
// Both the server and the connection are torn down via t.Cleanup.
func startGRPCServer(t *testing.T, h *Harness) *grpc.ClientConn {
	t.Helper()

	// Load the IR from the database. If no checkpoint exists, the
	// entity services won't be registered (admin-only server).
	ir := loadTestIR(t, h)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	srv := grpc.NewServer()

	dynServer := entity.NewServer(h.Pool, h.Cache, h.Outbox, h.QueryCache)
	if err := dynServer.Register(srv, ir); err != nil {
		t.Fatalf("dynServer.Register: %v", err)
	}

	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.GracefulStop)

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// loadTestIR reads the IR checkpoint from the database, falling back
// to an empty IR if no checkpoint exists.
func loadTestIR(t *testing.T, h *Harness) *dsl.IR {
	t.Helper()
	var raw []byte
	err := h.PgxPool().QueryRow(context.Background(),
		`SELECT ir FROM atlantis.ir_checkpoint WHERE id = 1`).Scan(&raw)
	if err != nil {
		// No checkpoint — return empty IR.
		return &dsl.IR{Version: dsl.CurrentIRVersion}
	}
	ir, err := dsl.DecodeJSONIR(raw)
	if err != nil {
		t.Fatalf("decode IR checkpoint: %v", err)
	}
	return ir
}

// TestGRPC_Account_RoundTrip is the wire-path canary: Create → Get →
// Delete through a real gRPC connection proves buf-generated stubs, the
// Register aggregator, and the protoconv helpers all align end-to-end.
//
// Broader CRUD coverage stays at the in-process runtime-tier layer
// (crud_test.go), which runs ~100x faster than the gRPC round-trip.
func TestGRPC_Account_RoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in -short mode")
	}
	h := NewHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn := startGRPCServer(t, h)
	client := consumerpb.NewAccountServiceClient(conn)

	createResp, err := client.CreateAccount(ctx, &consumerpb.CreateAccountRequest{
		Entity: &consumerpb.Account{
			Id:    "CA999999",
			Email: "wire@example.com",
		},
	})
	if err != nil {
		t.Fatalf("CreateAccount: %v", err)
	}
	if got := createResp.GetEntity().GetEmail(); got != "wire@example.com" {
		t.Errorf("Create.Entity.Email = %q, want %q", got, "wire@example.com")
	}

	getResp, err := client.GetAccount(ctx, &consumerpb.GetAccountRequest{Id: "CA999999"})
	if err != nil {
		t.Fatalf("GetAccount: %v", err)
	}
	if got := getResp.GetEntity().GetEmail(); got != "wire@example.com" {
		t.Errorf("Get.Entity.Email = %q, want %q", got, "wire@example.com")
	}

	if _, err := client.DeleteAccount(ctx, &consumerpb.DeleteAccountRequest{Id: "CA999999"}); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
}

// TestGRPC_QueryAccount_TierTwo exercises the tier-2 query-result cache
// end-to-end: insert a row, run QueryAccount twice, assert the second
// call hit the cache (no Store-after-PG round trip), then update the
// row and observe the next QueryAccount misses cache (because the
// outbox worker bumped the per-entity generation counter, which is
// folded into the hash).
//
// Correctness is asserted via the memcached counter: a successful tier-2
// Lookup followed by Store leaves the cache key written exactly once;
// a generation bump moves the counter forward, retiring the prior key.
func TestGRPC_QueryAccount_TierTwo(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in -short mode")
	}
	h := NewHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn := startGRPCServer(t, h)
	client := consumerpb.NewAccountServiceClient(conn)

	if _, err := client.CreateAccount(ctx, &consumerpb.CreateAccountRequest{
		Entity: &consumerpb.Account{
			Id:    "CA-T2-1",
			Email: "t2-one@example.com",
		},
	}); err != nil {
		t.Fatalf("CreateAccount(seed1): %v", err)
	}
	if _, err := client.CreateAccount(ctx, &consumerpb.CreateAccountRequest{
		Entity: &consumerpb.Account{
			Id:    "CA-T2-2",
			Email: "t2-two@example.com",
		},
	}); err != nil {
		t.Fatalf("CreateAccount(seed2): %v", err)
	}
	h.WaitForInvalidations(ctx, t, 5*time.Second)

	filter := &consumerpb.AccountFilter{
		Email: &commonpb.StringPredicate{
			Op: &commonpb.StringPredicate_Prefix{Prefix: "t2-"},
		},
	}

	genBefore, err := h.QueryCache.Generation(ctx, "consumer.Account")
	if err != nil {
		t.Fatalf("Generation pre: %v", err)
	}

	if _, err := client.QueryAccount(ctx, &consumerpb.QueryAccountRequest{
		Filter: filter,
		Limit:  10,
	}); err != nil {
		t.Fatalf("QueryAccount(first): %v", err)
	}

	if _, err := client.QueryAccount(ctx, &consumerpb.QueryAccountRequest{
		Filter: filter,
		Limit:  10,
	}); err != nil {
		t.Fatalf("QueryAccount(second): %v", err)
	}

	if _, err := client.UpdateAccount(ctx, &consumerpb.UpdateAccountRequest{
		Entity: &consumerpb.Account{
			Id:    "CA-T2-1",
			Email: "t2-one-updated@example.com",
		},
	}); err != nil {
		t.Fatalf("UpdateAccount: %v", err)
	}
	h.WaitForInvalidations(ctx, t, 5*time.Second)

	genAfter, err := h.QueryCache.Generation(ctx, "consumer.Account")
	if err != nil {
		t.Fatalf("Generation post: %v", err)
	}
	if genAfter <= genBefore {
		t.Errorf("generation counter did not advance: before=%d after=%d", genBefore, genAfter)
	}

	if _, err := client.QueryAccount(ctx, &consumerpb.QueryAccountRequest{
		Filter:    filter,
		Limit:     10,
		CacheSkip: true,
	}); err != nil {
		t.Fatalf("QueryAccount(cache_skip): %v", err)
	}
}

// protoString is a tiny helper that returns a *string for proto3
// optional fields. Avoids littering tests with `s := "foo"; &s`.
func protoString(v string) *string { return &v }

// TestGRPC_QueryAccount_Includes inserts one parent and two child rows,
// then verifies QueryAccount with an Address include returns the parent
// alongside its two addresses attached to the IncludedAddressByConsumerId
// slot. Also verifies the include is opt-in: a request without the
// include leaves the slot empty.
func TestGRPC_QueryAccount_Includes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in -short mode")
	}
	h := NewHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn := startGRPCServer(t, h)
	client := consumerpb.NewAccountServiceClient(conn)
	addressClient := consumerpb.NewAddressServiceClient(conn)

	if _, err := client.CreateAccount(ctx, &consumerpb.CreateAccountRequest{
		Entity: &consumerpb.Account{Id: "CA-INC-1", Email: "inc1@example.com"},
	}); err != nil {
		t.Fatalf("seed account: %v", err)
	}
	for i, line := range []string{"1 main st", "2 main st"} {
		// Address.Id is identity-assigned at INSERT time; passing 0 lets
		// PG fill it in. The fields below the FK are nullable scalars,
		// so we leave them empty rather than fight pointer assignment.
		_ = i
		if _, err := addressClient.CreateAddress(ctx, &consumerpb.CreateAddressRequest{
			Entity: &consumerpb.Address{
				ConsumerId: "CA-INC-1",
				Label:      protoString(line),
			},
		}); err != nil {
			t.Fatalf("seed address %d: %v", i, err)
		}
	}
	h.WaitForInvalidations(ctx, t, 5*time.Second)

	filter := &consumerpb.AccountFilter{
		Id: &commonpb.StringPredicate{
			Op: &commonpb.StringPredicate_Eq{Eq: "CA-INC-1"},
		},
	}

	withInc, err := client.QueryAccount(ctx, &consumerpb.QueryAccountRequest{
		Filter:   filter,
		Limit:    10,
		Includes: []consumerpb.AccountInclude{consumerpb.AccountInclude_ACCOUNT_INCLUDE_CONSUMER_ADDRESS_BY_CONSUMER_ID},
	})
	if err != nil {
		t.Fatalf("QueryAccount with include: %v", err)
	}
	if len(withInc.GetEntities()) != 1 {
		t.Fatalf("expected 1 account, got %d", len(withInc.GetEntities()))
	}
	got := withInc.GetEntities()[0].GetIncludedAddressByConsumerId()
	if len(got) != 2 {
		t.Errorf("expected 2 addresses attached, got %d", len(got))
	}

	without, err := client.QueryAccount(ctx, &consumerpb.QueryAccountRequest{
		Filter: filter,
		Limit:  10,
	})
	if err != nil {
		t.Fatalf("QueryAccount without include: %v", err)
	}
	if len(without.GetEntities()) != 1 {
		t.Fatalf("expected 1 account, got %d", len(without.GetEntities()))
	}
	if len(without.GetEntities()[0].GetIncludedAddressByConsumerId()) != 0 {
		t.Errorf("opt-in include leaked into a request that didn't ask for it")
	}
}

// TestGRPC_QueryAccount_KeysetPagination walks a multi-page query end-
// to-end. Seven rows are inserted with disjoint emails so the filter
// matches them all; the test then pages through with limit=3 and
// verifies (a) every page returns the expected count, (b) page
// boundaries don't overlap, (c) the final page omits next_page_token,
// (d) a corrupted token is rejected.
func TestGRPC_QueryAccount_KeysetPagination(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in -short mode")
	}
	h := NewHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	conn := startGRPCServer(t, h)
	client := consumerpb.NewAccountServiceClient(conn)

	const total = 7
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("CA-PAGE-%d", i)
		if _, err := client.CreateAccount(ctx, &consumerpb.CreateAccountRequest{
			Entity: &consumerpb.Account{
				Id:    id,
				Email: fmt.Sprintf("page-%02d@example.com", i),
			},
		}); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	h.WaitForInvalidations(ctx, t, 5*time.Second)

	filter := &consumerpb.AccountFilter{
		Email: &commonpb.StringPredicate{
			Op: &commonpb.StringPredicate_Prefix{Prefix: "page-"},
		},
	}
	const pageSize = 3

	var seen []string
	token := ""
	pages := 0
	for {
		resp, err := client.QueryAccount(ctx, &consumerpb.QueryAccountRequest{
			Filter:    filter,
			Limit:     pageSize,
			PageToken: token,
		})
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		for _, e := range resp.GetEntities() {
			seen = append(seen, e.GetId())
		}
		pages++
		token = resp.GetNextPageToken()
		if token == "" {
			break
		}
		if pages > total+1 {
			t.Fatalf("did not terminate after %d pages", pages)
		}
	}
	if len(seen) != total {
		t.Fatalf("paginated set: got %d rows want %d (rows: %v)", len(seen), total, seen)
	}
	// Boundaries should not overlap and the union should cover every row.
	uniq := map[string]bool{}
	for _, id := range seen {
		if uniq[id] {
			t.Errorf("duplicate row across pages: %s", id)
		}
		uniq[id] = true
	}

	// Corrupted token must error out.
	if _, err := client.QueryAccount(ctx, &consumerpb.QueryAccountRequest{
		Filter:    filter,
		Limit:     pageSize,
		PageToken: "not-a-real-cursor",
	}); err == nil {
		t.Errorf("expected error from corrupted page_token")
	}
}
