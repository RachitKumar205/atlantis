package interceptors

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/rachitkumar205/atlantis/internal/runtime"
)

func TestStatusMapsTheHandlerError(t *testing.T) {
	info := &grpc.UnaryServerInfo{FullMethod: "/atlantis.x.v1.NoteService/GetNote"}
	_, err := NewStatus()(context.Background(), nil, info, func(context.Context, any) (any, error) {
		return nil, runtime.ErrNotFound
	})
	if st, _ := status.FromError(err); st.Code() != codes.NotFound {
		t.Errorf("code = %v, want NotFound", st.Code())
	}
}

// TestStatusMapsBeforeTheOuterInterceptorSeesIt runs NewStatus inside another
// interceptor: the outer one observes the mapped code. Whether main.go places
// NewStatus last is not asserted here.
func TestStatusMapsBeforeTheOuterInterceptorSeesIt(t *testing.T) {
	var seen codes.Code
	outer := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		resp, err := h(ctx, req)
		seen = status.Code(err)
		return resp, err
	}
	chain := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		return outer(ctx, req, info, func(ctx context.Context, req any) (any, error) {
			return NewStatus()(ctx, req, info, h)
		})
	}
	_, _ = chain(context.Background(), nil, &grpc.UnaryServerInfo{}, func(context.Context, any) (any, error) {
		return nil, errors.New("plain")
	})
	if seen != codes.Internal {
		t.Errorf("outer interceptor saw %v, want Internal", seen)
	}
}

func TestStatusStreamMapsTheHandlerError(t *testing.T) {
	err := NewStatusStream()(nil, nil, &grpc.StreamServerInfo{}, func(any, grpc.ServerStream) error {
		return context.DeadlineExceeded
	})
	if status.Code(err) != codes.DeadlineExceeded {
		t.Errorf("code = %v, want DeadlineExceeded", status.Code(err))
	}
}
