package admin

import (
	"context"

	"google.golang.org/grpc"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

// The generated Admin service. Served alongside the hand-rolled JSON
// descriptor in grpc.go while clients migrate — see RegisterGenerated.
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
// Server-side migration is incremental; client-side migration is not. A client
// dialing with grpc.ForceCodecV2(jsonCodec{}) sets that as a *connection*-wide
// default, and grpc-go derives the content-subtype from the forced codec's name
// for every call on that connection. A client that switched one RPC to a
// generated stub over its existing connection would send content-subtype
// "json", the server would select the globally registered jsonCodec for a
// protobuf handler, and the call would fail Internal. Each client therefore
// moves wholesale, on its own dial — which is also how a released binary is
// deployed anyway.
//
// tide and tidectl have moved. internal/console/client.go is the last one still
// forcing the JSON codec, and grpc.go stays registered until it follows.
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
// Both this and Register (the hand-rolled JSON descriptor) are served while
// clients migrate. The capability interceptor governs both paths — its policy
// carries the legacy prefix alongside the generated one precisely so that
// serving two descriptors does not mean enforcing on one of them.
//
// Anything added here must be unary. The capability interceptor has no stream
// form, so the first streaming admin RPC would be served by cmd/server's stream
// chain and authorized by nothing. Nothing here would fail to compile, which is
// why authz.TestAdminServiceDeclaresNoStreamingRPC exists.
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
	"StartWorkflow":          true,
	"GetWorkflowStatus":      true,
	"BeginBackfillPlan":      true,
	"GetBackfillStatus":      true,
	"AdoptBaseline":          true,
	"PlanSchema":             true,
	"ApplyMigration":         true,
	"GetMergedSchema":        true,
	"GetCanonicalIR":         true,
	"GetCallerFiles":         true,
	"GetSchemaHistory":       true,
	"GetSchemaVersion":       true,
	"DiffSchemaVersions":     true,
	"GetEntityLineage":       true,
	"GetEntityOwners":        true,
	"RollbackSchema":         true,
	"PreviewRollback":        true,
	"GetLogs":                true,
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

func (g *grpcServer) StartWorkflow(ctx context.Context, req *adminpb.StartWorkflowRequest) (*adminpb.StartWorkflowResponse, error) {
	return g.svc.StartWorkflow(ctx, req)
}

func (g *grpcServer) GetWorkflowStatus(ctx context.Context, req *adminpb.GetWorkflowStatusRequest) (*adminpb.GetWorkflowStatusResponse, error) {
	return g.svc.GetWorkflowStatus(ctx, req)
}

func (g *grpcServer) BeginBackfillPlan(ctx context.Context, req *adminpb.BeginBackfillPlanRequest) (*adminpb.BeginBackfillPlanResponse, error) {
	return g.svc.BeginBackfillPlan(ctx, req)
}

func (g *grpcServer) GetBackfillStatus(ctx context.Context, req *adminpb.GetBackfillStatusRequest) (*adminpb.GetBackfillStatusResponse, error) {
	return g.svc.GetBackfillStatus(ctx, req)
}

func (g *grpcServer) AdoptBaseline(ctx context.Context, req *adminpb.AdoptBaselineRequest) (*adminpb.AdoptBaselineResponse, error) {
	return g.svc.AdoptBaseline(ctx, req)
}

func (g *grpcServer) PlanSchema(ctx context.Context, req *adminpb.PlanSchemaRequest) (*adminpb.PlanSchemaResponse, error) {
	return g.svc.PlanSchema(ctx, req)
}

func (g *grpcServer) ApplyMigration(ctx context.Context, req *adminpb.ApplyMigrationRequest) (*adminpb.ApplyMigrationResponse, error) {
	return g.svc.ApplyMigration(ctx, req)
}

func (g *grpcServer) GetMergedSchema(ctx context.Context, req *adminpb.GetMergedSchemaRequest) (*adminpb.GetMergedSchemaResponse, error) {
	return g.svc.GetMergedSchema(ctx, req)
}

func (g *grpcServer) GetCanonicalIR(ctx context.Context, req *adminpb.GetCanonicalIRRequest) (*adminpb.GetCanonicalIRResponse, error) {
	return g.svc.GetCanonicalIR(ctx, req)
}

func (g *grpcServer) GetCallerFiles(ctx context.Context, req *adminpb.GetCallerFilesRequest) (*adminpb.GetCallerFilesResponse, error) {
	return g.svc.GetCallerFiles(ctx, req)
}

func (g *grpcServer) GetSchemaHistory(ctx context.Context, req *adminpb.GetSchemaHistoryRequest) (*adminpb.GetSchemaHistoryResponse, error) {
	return g.svc.GetSchemaHistory(ctx, req)
}

func (g *grpcServer) GetSchemaVersion(ctx context.Context, req *adminpb.GetSchemaVersionRequest) (*adminpb.GetSchemaVersionResponse, error) {
	return g.svc.GetSchemaVersion(ctx, req)
}

func (g *grpcServer) DiffSchemaVersions(ctx context.Context, req *adminpb.DiffSchemaVersionsRequest) (*adminpb.DiffSchemaVersionsResponse, error) {
	return g.svc.DiffSchemaVersions(ctx, req)
}

func (g *grpcServer) GetEntityLineage(ctx context.Context, req *adminpb.GetEntityLineageRequest) (*adminpb.GetEntityLineageResponse, error) {
	return g.svc.GetEntityLineage(ctx, req)
}

func (g *grpcServer) GetEntityOwners(ctx context.Context, req *adminpb.GetEntityOwnersRequest) (*adminpb.GetEntityOwnersResponse, error) {
	return g.svc.GetEntityOwners(ctx, req)
}

func (g *grpcServer) RollbackSchema(ctx context.Context, req *adminpb.RollbackSchemaRequest) (*adminpb.RollbackSchemaResponse, error) {
	return g.svc.RollbackSchema(ctx, req)
}

func (g *grpcServer) PreviewRollback(ctx context.Context, req *adminpb.PreviewRollbackRequest) (*adminpb.PreviewRollbackResponse, error) {
	return g.svc.PreviewRollback(ctx, req)
}

func (g *grpcServer) GetLogs(ctx context.Context, req *adminpb.GetLogsRequest) (*adminpb.GetLogsResponse, error) {
	return g.svc.GetLogs(ctx, req)
}
