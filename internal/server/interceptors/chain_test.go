package interceptors

import (
	"context"
	"reflect"
	"testing"

	"google.golang.org/grpc"
)

type ctxKey string

func TestChainUnary_FirstRunsOutermost(t *testing.T) {
	var order []string
	mk := func(name string) grpc.UnaryServerInterceptor {
		return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			order = append(order, "enter "+name)
			ctx = context.WithValue(ctx, ctxKey(name), true)
			resp, err := handler(ctx, req)
			order = append(order, "leave "+name)
			return resp, err
		}
	}
	var sawAll bool
	chain := ChainUnary(mk("a"), mk("b"), mk("c"))
	resp, err := chain(context.Background(), "req", &grpc.UnaryServerInfo{FullMethod: "/x/Y"},
		func(ctx context.Context, req any) (any, error) {
			order = append(order, "handler")
			sawAll = ctx.Value(ctxKey("a")) != nil && ctx.Value(ctxKey("b")) != nil && ctx.Value(ctxKey("c")) != nil
			return "resp", nil
		})
	if err != nil || resp != "resp" {
		t.Fatalf("resp=%v err=%v", resp, err)
	}
	want := []string{"enter a", "enter b", "enter c", "handler", "leave c", "leave b", "leave a"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
	if !sawAll {
		t.Errorf("handler did not see every interceptor's context")
	}
}

func TestChainUnary_Empty(t *testing.T) {
	called := false
	_, err := ChainUnary()(context.Background(), nil, &grpc.UnaryServerInfo{}, func(context.Context, any) (any, error) {
		called = true
		return nil, nil
	})
	if err != nil || !called {
		t.Fatalf("empty chain: called=%v err=%v", called, err)
	}
}

type nopStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s nopStream) Context() context.Context { return s.ctx }

func TestChainStream_FirstRunsOutermost(t *testing.T) {
	var order []string
	mk := func(name string) grpc.StreamServerInterceptor {
		return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
			order = append(order, "enter "+name)
			err := handler(srv, ss)
			order = append(order, "leave "+name)
			return err
		}
	}
	err := ChainStream(mk("a"), mk("b"))(nil, nopStream{ctx: context.Background()}, &grpc.StreamServerInfo{FullMethod: "/x/Y"},
		func(any, grpc.ServerStream) error {
			order = append(order, "handler")
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"enter a", "enter b", "handler", "leave b", "leave a"}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
}

func TestStreamChainUnless(t *testing.T) {
	var chainRuns, handlerRuns int
	count := func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		chainRuns++
		return handler(srv, ss)
	}
	skipDynamic := func(m string) bool { return m == "/atlantis.x.v1.YService/GetY" }
	intr := StreamChainUnless(skipDynamic, count, count)
	handler := func(any, grpc.ServerStream) error { handlerRuns++; return nil }

	if err := intr(nil, nopStream{ctx: context.Background()}, &grpc.StreamServerInfo{FullMethod: "/atlantis.x.v1.YService/GetY"}, handler); err != nil {
		t.Fatal(err)
	}
	if chainRuns != 0 || handlerRuns != 1 {
		t.Errorf("skipped method: chain ran %d times, handler %d; want 0 and 1", chainRuns, handlerRuns)
	}
	if err := intr(nil, nopStream{ctx: context.Background()}, &grpc.StreamServerInfo{FullMethod: "/other/Z"}, handler); err != nil {
		t.Fatal(err)
	}
	if chainRuns != 2 || handlerRuns != 2 {
		t.Errorf("other method: chain ran %d times, handler %d; want 2 and 2", chainRuns, handlerRuns)
	}
}
