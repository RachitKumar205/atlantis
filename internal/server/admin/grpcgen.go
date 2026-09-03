package admin

import (
	"context"

	"google.golang.org/grpc"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

// The generated Admin service.
//
// Every RPC takes and returns generated types.
//
// A dual-serve window is constrained on the client side. A client dialing with
// grpc.ForceCodecV2 sets the codec as a connection-wide default, and grpc-go
// derives the content-subtype from it for every call on that connection, so a
// client cannot move one RPC at a time over an existing dial. Each client moves
// wholesale, on its own dial, which is how a released binary is deployed.
//
// UnimplementedAdminServiceServer is embedded, so adding an RPC to the proto
// and not implementing it here is a runtime Unimplemented rather than a build
// failure. TestMigratedRPCsAreTracked enumerates the descriptor and fails on
// any method neither implemented nor listed as pending.
type grpcServer struct {
	adminpb.UnimplementedAdminServiceServer
	svc *Service
}

// RegisterGenerated binds the protobuf Admin service to a gRPC server.
//
// This is the only admin registration. A hand-rolled JSON descriptor was served
// alongside it during the client migration; it is gone, along with the policy
// prefix that governed it.
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
	"GetChangePolicy":        true,
	"ListSchemaPlans":        true,
	"GetSchemaPlan":          true,
	"ApproveSchemaPlan":      true,
	"RejectSchemaPlan":       true,
	"SetChangePolicy":        true,
	"GetCallerAliases":       true,
	"SetCallerAliases":       true,
	"GetCallers":             true,
	"GetApplyPolicy":         true,
	"SetApplyPolicy":         true,
	"RehearseMigration":      true,
	"ListRehearsals":         true,
	"GetRehearsal":           true,
	"GetRehearsalDatabase":   true,
	"SetRehearsalDatabase":   true,
	"ListProtectedEntities":  true,
	"PutProtectedEntity":     true,
	"DeleteProtectedEntity":  true,
	"ListFreezeWindows":      true,
	"CreateFreezeWindow":     true,
	"DeleteFreezeWindow":     true,
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
	"SetManagedDatabase":     true,
	"InspectSchema":          true,
	"GenerateSchema":         true,
	"PlanSchema":             true,
	"ApplyMigration":         true,
	"GetMergedSchema":        true,
	"GetCanonicalIR":         true,
	"GetCallerFiles":         true,
	"ListParkedObjects":      true,
	"GetSchemaHistory":       true,
	"GetSchemaVersion":       true,
	"DiffSchemaVersions":     true,
	"GetEntityLineage":       true,
	"GetEntityOwners":        true,
	"RollbackSchema":         true,
	"PreviewRollback":        true,
	"GetLogs":                true,
}

// The methods below are plain forwards. Conversion between wire types and
// domain types lives on *Service rather than in a translation layer, which
// would be a fourth copy of every shape.

func (g *grpcServer) GetCallerAliases(ctx context.Context, req *adminpb.GetCallerAliasesRequest) (*adminpb.GetCallerAliasesResponse, error) {
	return g.svc.GetCallerAliases(ctx, req)
}

func (g *grpcServer) SetCallerAliases(ctx context.Context, req *adminpb.SetCallerAliasesRequest) (*adminpb.SetCallerAliasesResponse, error) {
	return g.svc.SetCallerAliases(ctx, req)
}

func (g *grpcServer) GetCallers(ctx context.Context, req *adminpb.GetCallersRequest) (*adminpb.GetCallersResponse, error) {
	return g.svc.GetCallers(ctx, req)
}

func (g *grpcServer) GetApplyPolicy(ctx context.Context, req *adminpb.GetApplyPolicyRequest) (*adminpb.GetApplyPolicyResponse, error) {
	return g.svc.GetApplyPolicy(ctx, req)
}

func (g *grpcServer) SetApplyPolicy(ctx context.Context, req *adminpb.SetApplyPolicyRequest) (*adminpb.SetApplyPolicyResponse, error) {
	return g.svc.SetApplyPolicy(ctx, req)
}

func (g *grpcServer) RehearseMigration(ctx context.Context, req *adminpb.RehearseMigrationRequest) (*adminpb.RehearseMigrationResponse, error) {
	return g.svc.RehearseMigration(ctx, req)
}

func (g *grpcServer) ListRehearsals(ctx context.Context, req *adminpb.ListRehearsalsRequest) (*adminpb.ListRehearsalsResponse, error) {
	return g.svc.ListRehearsals(ctx, req)
}

func (g *grpcServer) GetRehearsal(ctx context.Context, req *adminpb.GetRehearsalRequest) (*adminpb.GetRehearsalResponse, error) {
	return g.svc.GetRehearsal(ctx, req)
}

func (g *grpcServer) GetRehearsalDatabase(ctx context.Context, req *adminpb.GetRehearsalDatabaseRequest) (*adminpb.GetRehearsalDatabaseResponse, error) {
	return g.svc.GetRehearsalDatabase(ctx, req)
}

func (g *grpcServer) SetRehearsalDatabase(ctx context.Context, req *adminpb.SetRehearsalDatabaseRequest) (*adminpb.SetRehearsalDatabaseResponse, error) {
	return g.svc.SetRehearsalDatabase(ctx, req)
}

func (g *grpcServer) ListProtectedEntities(ctx context.Context, req *adminpb.ListProtectedEntitiesRequest) (*adminpb.ListProtectedEntitiesResponse, error) {
	return g.svc.ListProtectedEntities(ctx, req)
}

func (g *grpcServer) PutProtectedEntity(ctx context.Context, req *adminpb.PutProtectedEntityRequest) (*adminpb.PutProtectedEntityResponse, error) {
	return g.svc.PutProtectedEntity(ctx, req)
}

func (g *grpcServer) DeleteProtectedEntity(ctx context.Context, req *adminpb.DeleteProtectedEntityRequest) (*adminpb.DeleteProtectedEntityResponse, error) {
	return g.svc.DeleteProtectedEntity(ctx, req)
}

func (g *grpcServer) ListFreezeWindows(ctx context.Context, req *adminpb.ListFreezeWindowsRequest) (*adminpb.ListFreezeWindowsResponse, error) {
	return g.svc.ListFreezeWindows(ctx, req)
}

func (g *grpcServer) CreateFreezeWindow(ctx context.Context, req *adminpb.CreateFreezeWindowRequest) (*adminpb.CreateFreezeWindowResponse, error) {
	return g.svc.CreateFreezeWindow(ctx, req)
}

func (g *grpcServer) DeleteFreezeWindow(ctx context.Context, req *adminpb.DeleteFreezeWindowRequest) (*adminpb.DeleteFreezeWindowResponse, error) {
	return g.svc.DeleteFreezeWindow(ctx, req)
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

func (g *grpcServer) ListParkedObjects(ctx context.Context, req *adminpb.ListParkedObjectsRequest) (*adminpb.ListParkedObjectsResponse, error) {
	return g.svc.ListParkedObjects(ctx, req)
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

func (g *grpcServer) SetManagedDatabase(ctx context.Context, req *adminpb.SetManagedDatabaseRequest) (*adminpb.SetManagedDatabaseResponse, error) {
	return g.svc.SetManagedDatabase(ctx, req)
}

func (g *grpcServer) InspectSchema(ctx context.Context, req *adminpb.InspectSchemaRequest) (*adminpb.InspectSchemaResponse, error) {
	return g.svc.InspectSchema(ctx, req)
}

func (g *grpcServer) GenerateSchema(ctx context.Context, req *adminpb.GenerateSchemaRequest) (*adminpb.GenerateSchemaResponse, error) {
	return g.svc.GenerateSchema(ctx, req)
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

func (g *grpcServer) GetChangePolicy(ctx context.Context, req *adminpb.GetChangePolicyRequest) (*adminpb.GetChangePolicyResponse, error) {
	return g.svc.GetChangePolicy(ctx, req)
}

func (g *grpcServer) SetChangePolicy(ctx context.Context, req *adminpb.SetChangePolicyRequest) (*adminpb.SetChangePolicyResponse, error) {
	return g.svc.SetChangePolicy(ctx, req)
}

func (g *grpcServer) ListSchemaPlans(ctx context.Context, req *adminpb.ListSchemaPlansRequest) (*adminpb.ListSchemaPlansResponse, error) {
	return g.svc.ListSchemaPlans(ctx, req)
}

func (g *grpcServer) GetSchemaPlan(ctx context.Context, req *adminpb.GetSchemaPlanRequest) (*adminpb.GetSchemaPlanResponse, error) {
	return g.svc.GetSchemaPlan(ctx, req)
}

func (g *grpcServer) ApproveSchemaPlan(ctx context.Context, req *adminpb.ApproveSchemaPlanRequest) (*adminpb.ApproveSchemaPlanResponse, error) {
	return g.svc.ApproveSchemaPlan(ctx, req)
}

func (g *grpcServer) RejectSchemaPlan(ctx context.Context, req *adminpb.RejectSchemaPlanRequest) (*adminpb.RejectSchemaPlanResponse, error) {
	return g.svc.RejectSchemaPlan(ctx, req)
}

func (g *grpcServer) GetLogs(ctx context.Context, req *adminpb.GetLogsRequest) (*adminpb.GetLogsResponse, error) {
	return g.svc.GetLogs(ctx, req)
}
