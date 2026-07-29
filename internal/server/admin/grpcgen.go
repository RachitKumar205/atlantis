package admin

import (
	"context"

	"google.golang.org/grpc"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

// The generated Admin service. Built and tested, but not yet served in
// production — see RegisterGenerated.
//
// The wire format moves from a JSON envelope to protobuf and the service path
// moves with it, so the two cannot be swapped in one change without breaking
// every deployed tide at the moment the server restarts. RPCs are reshaped on
// *Service a slice at a time, with the handlers in grpc.go converting for as
// long as the JSON path survives, and this type accumulating the migrated ones.
//
// Two constraints on that plan are worth stating, because both were assumed
// away in an earlier draft of this comment.
//
// Server-side migration is incremental; client-side migration is not. Every
// client dials with grpc.ForceCodecV2(jsonCodec{}) as a *connection*-wide
// default option (cmd/tide/client.go, cmd/tidectl/client.go,
// internal/console/client.go), and grpc-go derives the content-subtype from
// the forced codec's name for every call on that connection. A client that
// switched one RPC to a generated stub over its existing connection would send
// content-subtype "json", the server would select the globally registered
// jsonCodec for a protobuf handler, and the call would fail Internal. Each
// client therefore moves wholesale, on its own dial, once the server side is
// complete — which is also how a released binary is deployed anyway.
//
// UnimplementedAdminServiceServer supplies the not-yet-migrated methods so
// this type satisfies the interface throughout rather than only at the end.
// That convenience costs the compile-time check the hand-rolled AdminServer
// interface provided: forgetting to add a method here is a runtime
// Unimplemented, not a build failure. TestMigratedRPCsAreTracked replaces the
// guarantee — it enumerates the descriptor and fails on any method that is
// neither migrated nor explicitly listed as pending.
//
// When the last RPC lands, grpc.go and the hand-written request/response
// structs are deleted and this type collapses into *Service.
type grpcServer struct {
	adminpb.UnimplementedAdminServiceServer
	svc *Service
}

// RegisterGenerated binds the protobuf Admin service to a gRPC server.
//
// cmd/server does not call this yet, and the omission is deliberate rather
// than pending cleanup. AuthChecker's exempt list covers
// "/atlantis.admin.v1.Admin/", which does not prefix-match
// "/atlantis.admin.v1.AdminService/", and the capability interceptor is not
// installed. Serving this path today would mean an endpoint gated only by
// authorizeOperator, which returns nil for every caller when
// ATL_OPERATOR_ALLOWED_CALLERS is unset — the shipped default, and the exact
// hole internal/server/authz exists to close. Registration lands with the
// interceptor. Tests call this directly.
func RegisterGenerated(srv *grpc.Server, svc *Service) {
	adminpb.RegisterAdminServiceServer(srv, &grpcServer{svc: svc})
}

// migratedRPCs names every method implemented on grpcServer. Keeping it beside
// the implementations lets a test hold the descriptor to account; see the note
// on UnimplementedAdminServiceServer above for why the compiler no longer can.
var migratedRPCs = map[string]bool{
	"GetCallerAliases": true,
	"SetCallerAliases": true,
}

// ---------------------------------------------------------------------------
// Migrated RPCs
//
// Each method here is a plain forward. The conversion between wire types and
// domain types belongs on *Service, not in a translation layer — a translation
// layer is the four-copies problem wearing a different hat, and it would have
// to be deleted again later.
// ---------------------------------------------------------------------------

func (g *grpcServer) GetCallerAliases(ctx context.Context, req *adminpb.GetCallerAliasesRequest) (*adminpb.GetCallerAliasesResponse, error) {
	return g.svc.GetCallerAliases(ctx, req)
}

func (g *grpcServer) SetCallerAliases(ctx context.Context, req *adminpb.SetCallerAliasesRequest) (*adminpb.SetCallerAliasesResponse, error) {
	return g.svc.SetCallerAliases(ctx, req)
}
