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
	"GetCallerAliases":       true,
	"SetCallerAliases":       true,
	"GetCallers":             true,
	"RegisterCaller":         true,
	"RevokeCaller":           true,
	"RecordCallerCertExpiry": true,
	"ListConnectedWorkers":   true,
	"GetWorkerSession":       true,
	"DrainWorker":            true,
	"EvictWorker":            true,
	"SubmitJob":              true,
	"GetJobStatus":           true,
	"ListDeadJobs":           true,
	"RetryDeadJob":           true,
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

func (g *grpcServer) GetCallers(ctx context.Context, req *adminpb.GetCallersRequest) (*adminpb.GetCallersResponse, error) {
	return g.svc.GetCallers(ctx, req)
}

func (g *grpcServer) RegisterCaller(ctx context.Context, req *adminpb.RegisterCallerRequest) (*adminpb.RegisterCallerResponse, error) {
	return g.svc.RegisterCaller(ctx, req)
}

func (g *grpcServer) RevokeCaller(ctx context.Context, req *adminpb.RevokeCallerRequest) (*adminpb.RevokeCallerResponse, error) {
	return g.svc.RevokeCaller(ctx, req)
}

func (g *grpcServer) RecordCallerCertExpiry(ctx context.Context, req *adminpb.RecordCallerCertExpiryRequest) (*adminpb.RecordCallerCertExpiryResponse, error) {
	return g.svc.RecordCallerCertExpiry(ctx, req)
}

func (g *grpcServer) ListConnectedWorkers(ctx context.Context, req *adminpb.ListConnectedWorkersRequest) (*adminpb.ListConnectedWorkersResponse, error) {
	return g.svc.ListConnectedWorkers(ctx, req)
}

func (g *grpcServer) GetWorkerSession(ctx context.Context, req *adminpb.GetWorkerSessionRequest) (*adminpb.GetWorkerSessionResponse, error) {
	return g.svc.GetWorkerSession(ctx, req)
}

func (g *grpcServer) DrainWorker(ctx context.Context, req *adminpb.DrainWorkerRequest) (*adminpb.DrainWorkerResponse, error) {
	return g.svc.DrainWorker(ctx, req)
}

func (g *grpcServer) EvictWorker(ctx context.Context, req *adminpb.EvictWorkerRequest) (*adminpb.EvictWorkerResponse, error) {
	return g.svc.EvictWorker(ctx, req)
}

func (g *grpcServer) SubmitJob(ctx context.Context, req *adminpb.SubmitJobRequest) (*adminpb.SubmitJobResponse, error) {
	return g.svc.SubmitJob(ctx, req)
}

func (g *grpcServer) GetJobStatus(ctx context.Context, req *adminpb.GetJobStatusRequest) (*adminpb.GetJobStatusResponse, error) {
	return g.svc.GetJobStatus(ctx, req)
}

func (g *grpcServer) ListDeadJobs(ctx context.Context, req *adminpb.ListDeadJobsRequest) (*adminpb.ListDeadJobsResponse, error) {
	return g.svc.ListDeadJobs(ctx, req)
}

func (g *grpcServer) RetryDeadJob(ctx context.Context, req *adminpb.RetryDeadJobRequest) (*adminpb.RetryDeadJobResponse, error) {
	return g.svc.RetryDeadJob(ctx, req)
}
