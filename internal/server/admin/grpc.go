package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"google.golang.org/grpc"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/mem"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

// jsonCodec mirrors the codec the console BFF installs on the client side.
// tide and tidectl no longer use it — they speak protobuf against the
// generated service — so this exists for internal/console alone and goes away
// with it.
// Registering here means a `tide` invocation can use
// `ForceCodecV2(jsonCodec{})` and the server will negotiate to JSON.
//
// The codec is registered once at package init.
type jsonCodec struct{}

func (jsonCodec) Marshal(v any) (mem.BufferSlice, error) {
	m, ok := v.(*jsonMsg)
	if !ok {
		return nil, fmt.Errorf("jsonCodec: cannot marshal %T", v)
	}
	return mem.BufferSlice{mem.SliceBuffer(m.Raw)}, nil
}

func (jsonCodec) Unmarshal(data mem.BufferSlice, v any) error {
	m, ok := v.(*jsonMsg)
	if !ok {
		return fmt.Errorf("jsonCodec: cannot unmarshal into %T", v)
	}
	m.Raw = append(m.Raw[:0], data.Materialize()...)
	return nil
}

func (jsonCodec) Name() string { return "json" }

func init() { encoding.RegisterCodecV2(jsonCodec{}) }

// gRPC wiring for the Admin service.
//
// Hand-rolled rather than buf-generated because the Admin service is
// internal infrastructure that bootstraps the entity codegen pipeline;
// wiring it to its own buf-generated client would either need a separate
// proto module or hit a chicken-and-egg with the entity emitter. The
// hand-rolled ServiceDesc uses JSON envelopes — the wire shape
// (POST /atlantis.admin.v1.Admin/RPC with a JSON body) is stable for the
// `tide` CLI.

// AdminServer is the typed interface gRPC's RegisterService machinery
// uses for runtime conformance checking. It mirrors what a buf-generated
// stub would emit — required because grpc.ServiceDesc.HandlerType must
// be a pointer to an interface (gRPC calls reflect.Type.Implements on it,
// which panics on a non-interface type). *Service satisfies this
// interface by carrying the same method set.
type AdminServer interface {
	PlanSchema(context.Context, *adminpb.PlanSchemaRequest) (*adminpb.PlanSchemaResponse, error)
	ApplyMigration(context.Context, *adminpb.ApplyMigrationRequest) (*adminpb.ApplyMigrationResponse, error)
	GetMergedSchema(context.Context, *adminpb.GetMergedSchemaRequest) (*adminpb.GetMergedSchemaResponse, error)
	GetCanonicalIR(context.Context, *adminpb.GetCanonicalIRRequest) (*adminpb.GetCanonicalIRResponse, error)
	BeginBackfillPlan(context.Context, *adminpb.BeginBackfillPlanRequest) (*adminpb.BeginBackfillPlanResponse, error)
	GetBackfillStatus(context.Context, *adminpb.GetBackfillStatusRequest) (*adminpb.GetBackfillStatusResponse, error)
	AdoptBaseline(context.Context, *adminpb.AdoptBaselineRequest) (*adminpb.AdoptBaselineResponse, error)
	SubmitJob(context.Context, *adminpb.SubmitJobRequest) (*adminpb.SubmitJobResponse, error)
	GetJobStatus(context.Context, *adminpb.GetJobStatusRequest) (*adminpb.GetJobStatusResponse, error)
	ListDeadJobs(context.Context, *adminpb.ListDeadJobsRequest) (*adminpb.ListDeadJobsResponse, error)
	RetryDeadJob(context.Context, *adminpb.RetryDeadJobRequest) (*adminpb.RetryDeadJobResponse, error)
	StartWorkflow(context.Context, *adminpb.StartWorkflowRequest) (*adminpb.StartWorkflowResponse, error)
	GetWorkflowStatus(context.Context, *adminpb.GetWorkflowStatusRequest) (*adminpb.GetWorkflowStatusResponse, error)
	GetSchemaHistory(context.Context, *adminpb.GetSchemaHistoryRequest) (*adminpb.GetSchemaHistoryResponse, error)
	GetSchemaVersion(context.Context, *adminpb.GetSchemaVersionRequest) (*adminpb.GetSchemaVersionResponse, error)
	DiffSchemaVersions(context.Context, *adminpb.DiffSchemaVersionsRequest) (*adminpb.DiffSchemaVersionsResponse, error)
	GetEntityLineage(context.Context, *adminpb.GetEntityLineageRequest) (*adminpb.GetEntityLineageResponse, error)
	GetEntityOwners(context.Context, *adminpb.GetEntityOwnersRequest) (*adminpb.GetEntityOwnersResponse, error)
	RollbackSchema(context.Context, *adminpb.RollbackSchemaRequest) (*adminpb.RollbackSchemaResponse, error)
	PreviewRollback(context.Context, *adminpb.PreviewRollbackRequest) (*adminpb.PreviewRollbackResponse, error)
	GetCallerFiles(context.Context, *adminpb.GetCallerFilesRequest) (*adminpb.GetCallerFilesResponse, error)
	GetCallers(context.Context, *adminpb.GetCallersRequest) (*adminpb.GetCallersResponse, error)
	RegisterCaller(context.Context, *adminpb.RegisterCallerRequest) (*adminpb.RegisterCallerResponse, error)
	RevokeCaller(context.Context, *adminpb.RevokeCallerRequest) (*adminpb.RevokeCallerResponse, error)
	RecordCallerCertExpiry(context.Context, *adminpb.RecordCallerCertExpiryRequest) (*adminpb.RecordCallerCertExpiryResponse, error)
	GetLogs(context.Context, *adminpb.GetLogsRequest) (*adminpb.GetLogsResponse, error)
	ListConnectedWorkers(context.Context, *adminpb.ListConnectedWorkersRequest) (*adminpb.ListConnectedWorkersResponse, error)
	GetWorkerSession(context.Context, *adminpb.GetWorkerSessionRequest) (*adminpb.GetWorkerSessionResponse, error)
	DrainWorker(context.Context, *adminpb.DrainWorkerRequest) (*adminpb.DrainWorkerResponse, error)
	EvictWorker(context.Context, *adminpb.EvictWorkerRequest) (*adminpb.EvictWorkerResponse, error)
	// Migrated to protobuf types; the JSON handlers above translate. As each
	// remaining RPC moves, its entry here changes shape too, until the whole
	// interface and this file are deleted.
	GetCallerAliases(context.Context, *adminpb.GetCallerAliasesRequest) (*adminpb.GetCallerAliasesResponse, error)
	SetCallerAliases(context.Context, *adminpb.SetCallerAliasesRequest) (*adminpb.SetCallerAliasesResponse, error)
}

// Compile-time check: *Service is the implementation of
// AdminServer. Adding an RPC = method on Service + entry in AdminServer +
// entry in serviceDesc.Methods. The compiler enforces all three stay in
// sync — drop one and this line won't compile.
var _ AdminServer = (*Service)(nil)

// Register binds the Admin service to a gRPC server.
func Register(srv *grpc.Server, svc *Service) {
	srv.RegisterService(&serviceDesc, svc)
}

// serviceDesc declares the wire-visible methods. Codec is JSON; gRPC sees
// each request/response as a `[]byte` it does not interpret. The handler
// unmarshals into our typed request structs, runs the logic, marshals the
// reply back.
var serviceDesc = grpc.ServiceDesc{
	ServiceName: "atlantis.admin.v1.Admin",
	HandlerType: (*AdminServer)(nil),
	Methods: []grpc.MethodDesc{
		{MethodName: "PlanSchema", Handler: handlePlanSchema},
		{MethodName: "ApplyMigration", Handler: handleApplyMigration},
		{MethodName: "GetMergedSchema", Handler: handleGetMergedSchema},
		{MethodName: "GetCanonicalIR", Handler: handleGetCanonicalIR},
		{MethodName: "BeginBackfillPlan", Handler: handleBeginBackfillPlan},
		{MethodName: "GetBackfillStatus", Handler: handleGetBackfillStatus},
		{MethodName: "AdoptBaseline", Handler: handleAdoptBaseline},
		{MethodName: "SubmitJob", Handler: handleSubmitJob},
		{MethodName: "GetJobStatus", Handler: handleGetJobStatus},
		{MethodName: "ListDeadJobs", Handler: handleListDeadJobs},
		{MethodName: "RetryDeadJob", Handler: handleRetryDeadJob},
		{MethodName: "StartWorkflow", Handler: handleStartWorkflow},
		{MethodName: "GetWorkflowStatus", Handler: handleGetWorkflowStatus},
		{MethodName: "GetSchemaHistory", Handler: handleGetSchemaHistory},
		{MethodName: "GetSchemaVersion", Handler: handleGetSchemaVersion},
		{MethodName: "DiffSchemaVersions", Handler: handleDiffSchemaVersions},
		{MethodName: "GetEntityLineage", Handler: handleGetEntityLineage},
		{MethodName: "GetEntityOwners", Handler: handleGetEntityOwners},
		{MethodName: "RollbackSchema", Handler: handleRollbackSchema},
		{MethodName: "PreviewRollback", Handler: handlePreviewRollback},
		{MethodName: "GetCallerFiles", Handler: handleGetCallerFiles},
		{MethodName: "GetCallers", Handler: handleGetCallers},
		{MethodName: "RegisterCaller", Handler: handleRegisterCaller},
		{MethodName: "RevokeCaller", Handler: handleRevokeCaller},
		{MethodName: "RecordCallerCertExpiry", Handler: handleRecordCallerCertExpiry},
		{MethodName: "GetLogs", Handler: handleGetLogs},
		{MethodName: "ListConnectedWorkers", Handler: handleListConnectedWorkers},
		{MethodName: "GetWorkerSession", Handler: handleGetWorkerSession},
		{MethodName: "DrainWorker", Handler: handleDrainWorker},
		{MethodName: "EvictWorker", Handler: handleEvictWorker},
		{MethodName: "GetCallerAliases", Handler: handleGetCallerAliases},
		{MethodName: "SetCallerAliases", Handler: handleSetCallerAliases},
	},
	Streams:  []grpc.StreamDesc{},
	Metadata: "atlantis/admin/v1/admin.proto",
}

// jsonMsg is the wire payload type. gRPC's installed jsonCodec calls
// Marshal / Unmarshal on this; the message itself carries raw JSON bytes.
type jsonMsg struct {
	Raw []byte
}

// handlePlanSchema is the gRPC entry. Decodes JSON → PlanRequest, calls the
// pure-Go Service.PlanSchema, encodes PlanResponse → JSON. Interceptors
// (mTLS, caller resolution, logging) run upstream of this handler.
func handlePlanSchema(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(jsonMsg)
	if err := dec(in); err != nil {
		return nil, err
	}
	var req PlanRequest
	if err := json.Unmarshal(in.Raw, &req); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return invokePlan(srv.(*Service), ctx, &req)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/atlantis.admin.v1.Admin/PlanSchema"}
	handler := func(ctx context.Context, _ any) (any, error) {
		return invokePlan(srv.(*Service), ctx, &req)
	}
	return interceptor(ctx, &req, info, handler)
}

// planResponseFromPB rebuilds the legacy JSON shape. Extracted so the mapping
// is testable: PlanResponse is the widest message in the service — 25 fields,
// most tagged omitempty — and an inline literal in the shim could drop any one
// of them without a test noticing.
func planResponseFromPB(p *adminpb.PlanSchemaResponse) *PlanResponse {
	return &PlanResponse{
		PlanID:          p.GetPlanId(),
		Class:           planClassFromPB(p.GetClass()),
		UpSQL:           p.GetUpSql(),
		DownSQL:         p.GetDownSql(),
		ImpactReport:    impactFromPB(p.GetImpactReport()),
		ParseErrors:     p.GetParseErrors(),
		BreakingDetail:  p.GetBreakingDetail(),
		CheckpointHash:  p.GetCheckpointHash(),
		CustomSQLErrors: p.GetCustomSqlErrors(),
		CustomCount: CustomDeclCount{
			Queries:    int(p.GetCustomCount().GetQueries()),
			Procedures: int(p.GetCustomCount().GetProcedures()),
		},
		PreBackfillUpSQL:       p.GetPreBackfillUpSql(),
		PreBackfillIndexesSQL:  p.GetPreBackfillIndexesSql(),
		PostBackfillUpSQL:      p.GetPostBackfillUpSql(),
		PostBackfillIndexesSQL: p.GetPostBackfillIndexesSql(),
		BackfillFields:         backfillFieldsFromPB(p.GetBackfillFields()),
		Extensions:             extensionsFromPB(p.GetExtensions()),
		IndexDrift:             indexDriftFromPB(p.GetIndexDrift()),
		IndexDriftNotes:        p.GetIndexDriftNotes(),
		IndexDriftError:        p.GetIndexDriftError(),
		CheckDrift:             checkDriftFromPB(p.GetCheckDrift()),
		CheckDriftNotes:        p.GetCheckDriftNotes(),
		CheckDriftError:        p.GetCheckDriftError(),
		ColumnDrift:            columnDriftFromPB(p.GetColumnDrift()),
		ColumnDriftNotes:       p.GetColumnDriftNotes(),
		ColumnDriftError:       p.GetColumnDriftError(),
	}
}

// Migration shim; see the note on invokeGetCallers.
func invokePlan(svc *Service, ctx context.Context, req *PlanRequest) (any, error) {
	pbResp, err := svc.PlanSchema(ctx, &adminpb.PlanSchemaRequest{
		Caller: req.Caller,
		Files:  submittedFilesToPB(req.Files),
	})
	if err != nil {
		return nil, err
	}
	resp := planResponseFromPB(pbResp)
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return &jsonMsg{Raw: raw}, nil
}

func handleApplyMigration(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(jsonMsg)
	if err := dec(in); err != nil {
		return nil, err
	}
	var req ApplyRequest
	if err := json.Unmarshal(in.Raw, &req); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return invokeApply(srv.(*Service), ctx, &req)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/atlantis.admin.v1.Admin/ApplyMigration"}
	handler := func(ctx context.Context, _ any) (any, error) {
		return invokeApply(srv.(*Service), ctx, &req)
	}
	return interceptor(ctx, &req, info, handler)
}

// Migration shim; see the note on invokeGetCallers.
//
// ApplyRequest.UpSQL is dropped rather than forwarded. It was documented as a
// drift check and never read by the server, which recomputes the SQL from the
// submitted files — the same property BeginBackfillPlan now enforces.
func invokeApply(svc *Service, ctx context.Context, req *ApplyRequest) (any, error) {
	pbResp, err := svc.ApplyMigration(ctx, &adminpb.ApplyMigrationRequest{
		Caller:         req.Caller,
		PlanId:         req.PlanID,
		Files:          submittedFilesToPB(req.Files),
		CheckpointHash: req.CheckpointHash,
	})
	if err != nil {
		return nil, err
	}
	resp := &ApplyResponse{
		AppliedAt:   pbResp.GetAppliedAt(),
		Version:     pbResp.GetVersion(),
		ContentHash: pbResp.GetContentHash(),
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return &jsonMsg{Raw: raw}, nil
}

// handleGetMergedSchema is the gRPC entry for the `tide pull` flow. Same
// JSON-envelope shape as the other admin RPCs.
func handleGetMergedSchema(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(jsonMsg)
	if err := dec(in); err != nil {
		return nil, err
	}
	var req GetMergedSchemaRequest
	// An empty body is a valid "give me everything" request — don't fail
	// when the client doesn't send the SinceVersion field.
	if len(in.Raw) > 0 {
		if err := json.Unmarshal(in.Raw, &req); err != nil {
			return nil, err
		}
	}
	if interceptor == nil {
		return invokeGetMergedSchema(srv.(*Service), ctx, &req)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/atlantis.admin.v1.Admin/GetMergedSchema"}
	handler := func(ctx context.Context, _ any) (any, error) {
		return invokeGetMergedSchema(srv.(*Service), ctx, &req)
	}
	return interceptor(ctx, &req, info, handler)
}

// Migration shim; see the note on invokeGetCallers.
func invokeGetMergedSchema(svc *Service, ctx context.Context, req *GetMergedSchemaRequest) (any, error) {
	pbResp, err := svc.GetMergedSchema(ctx, &adminpb.GetMergedSchemaRequest{SinceVersion: req.SinceVersion})
	if err != nil {
		return nil, err
	}
	resp := &GetMergedSchemaResponse{
		Version: pbResp.GetVersion(),
		Files:   submittedFilesFromPB(pbResp.GetFiles()),
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return &jsonMsg{Raw: raw}, nil
}

// handleGetCanonicalIR is the gRPC entry for the `tide generate` flow.
// Returns the checkpoint IR with proto numbers intact for caller-local
// SDK generation. Same JSON-envelope shape as the other admin RPCs.
func handleGetCanonicalIR(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(jsonMsg)
	if err := dec(in); err != nil {
		return nil, err
	}
	var req GetCanonicalIRRequest
	if len(in.Raw) > 0 {
		if err := json.Unmarshal(in.Raw, &req); err != nil {
			return nil, err
		}
	}
	if interceptor == nil {
		return invokeGetCanonicalIR(srv.(*Service), ctx, &req)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/atlantis.admin.v1.Admin/GetCanonicalIR"}
	handler := func(ctx context.Context, _ any) (any, error) {
		return invokeGetCanonicalIR(srv.(*Service), ctx, &req)
	}
	return interceptor(ctx, &req, info, handler)
}

// Migration shim; see the note on invokeGetCallers.
func invokeGetCanonicalIR(svc *Service, ctx context.Context, _ *GetCanonicalIRRequest) (any, error) {
	pbResp, err := svc.GetCanonicalIR(ctx, &adminpb.GetCanonicalIRRequest{})
	if err != nil {
		return nil, err
	}
	resp := &GetCanonicalIRResponse{
		IR:          json.RawMessage(pbResp.GetIr()),
		ContentHash: pbResp.GetContentHash(),
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return &jsonMsg{Raw: raw}, nil
}

func handleBeginBackfillPlan(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(jsonMsg)
	if err := dec(in); err != nil {
		return nil, err
	}
	var req BeginBackfillPlanRequest
	if err := json.Unmarshal(in.Raw, &req); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return invokeBeginBackfillPlan(srv.(*Service), ctx, &req)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/atlantis.admin.v1.Admin/BeginBackfillPlan"}
	handler := func(ctx context.Context, _ any) (any, error) {
		return invokeBeginBackfillPlan(srv.(*Service), ctx, &req)
	}
	return interceptor(ctx, &req, info, handler)
}

// beginBackfillRequestToPB drops the four SQL fields and the field list the
// legacy JSON request still carries, rather than forwarding them. Older tide
// binaries keep sending them and are unaffected: the server derives every
// statement from the submitted schema, so the values were only ever a
// restatement of what the .atl files already say.
//
// Extracted so a test can assert the drop. Inline, nothing would fail if a
// later edit re-added `PreBackfillUpSQL: req.PreBackfillUpSQL`.
func beginBackfillRequestToPB(req *BeginBackfillPlanRequest) *adminpb.BeginBackfillPlanRequest {
	return &adminpb.BeginBackfillPlanRequest{
		Caller: req.Caller,
		PlanId: req.PlanID,
		Files:  submittedFilesToPB(req.Files),
	}
}

// Migration shim; see the note on invokeGetCallers.
func invokeBeginBackfillPlan(svc *Service, ctx context.Context, req *BeginBackfillPlanRequest) (any, error) {
	pbResp, err := svc.BeginBackfillPlan(ctx, beginBackfillRequestToPB(req))
	if err != nil {
		return nil, err
	}
	resp := &BeginBackfillPlanResponse{
		PlanHash:        pbResp.GetPlanHash(),
		Accepted:        pbResp.GetAccepted(),
		AlreadyRunning:  pbResp.GetAlreadyRunning(),
		AlreadyComplete: pbResp.GetAlreadyComplete(),
		Message:         pbResp.GetMessage(),
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return &jsonMsg{Raw: raw}, nil
}

func handleGetBackfillStatus(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(jsonMsg)
	if err := dec(in); err != nil {
		return nil, err
	}
	var req GetBackfillStatusRequest
	if len(in.Raw) > 0 {
		if err := json.Unmarshal(in.Raw, &req); err != nil {
			return nil, err
		}
	}
	if interceptor == nil {
		return invokeGetBackfillStatus(srv.(*Service), ctx, &req)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/atlantis.admin.v1.Admin/GetBackfillStatus"}
	handler := func(ctx context.Context, _ any) (any, error) {
		return invokeGetBackfillStatus(srv.(*Service), ctx, &req)
	}
	return interceptor(ctx, &req, info, handler)
}

// Migration shim; see the note on invokeGetCallers.
func invokeGetBackfillStatus(svc *Service, ctx context.Context, req *GetBackfillStatusRequest) (any, error) {
	pbResp, err := svc.GetBackfillStatus(ctx, &adminpb.GetBackfillStatusRequest{
		PlanHash:        req.PlanHash,
		LatestForCaller: req.LatestForCaller,
	})
	if err != nil {
		return nil, err
	}
	resp := &GetBackfillStatusResponse{
		PlanHash:    pbResp.GetPlanHash(),
		Caller:      pbResp.GetCaller(),
		Status:      pbResp.GetStatus(),
		ErrorMsg:    pbResp.GetErrorMsg(),
		StartedAt:   pbResp.GetStartedAt(),
		CompletedAt: pbResp.GetCompletedAt(),
		Fields:      backfillFieldStatusFromPB(pbResp.GetFields()),
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return &jsonMsg{Raw: raw}, nil
}

func handleAdoptBaseline(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(jsonMsg)
	if err := dec(in); err != nil {
		return nil, err
	}
	var req AdoptBaselineRequest
	if err := json.Unmarshal(in.Raw, &req); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return invokeAdoptBaseline(srv.(*Service), ctx, &req)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/atlantis.admin.v1.Admin/AdoptBaseline"}
	handler := func(ctx context.Context, _ any) (any, error) {
		return invokeAdoptBaseline(srv.(*Service), ctx, &req)
	}
	return interceptor(ctx, &req, info, handler)
}

// Migration shim; see the note on invokeGetCallers.
func invokeAdoptBaseline(svc *Service, ctx context.Context, req *AdoptBaselineRequest) (any, error) {
	pbResp, err := svc.AdoptBaseline(ctx, adoptBaselineRequestToPB(req))
	if err != nil {
		return nil, err
	}
	resp := adoptBaselineResponseFromPB(pbResp)
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return &jsonMsg{Raw: raw}, nil
}

// handleSubmitJob, handleGetJobStatus, handleListDeadJobs, and
// handleRetryDeadJob are the gRPC entry points for the declarative-
// job admin surface. The shape mirrors every other RPC in this file
// — JSON-envelope codec, typed request/response struct, optional
// interceptor — so wire conformance is mechanical.

func handleSubmitJob(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(jsonMsg)
	if err := dec(in); err != nil {
		return nil, err
	}
	var req SubmitJobRequest
	if err := json.Unmarshal(in.Raw, &req); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return invokeSubmitJob(srv.(*Service), ctx, &req)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/atlantis.admin.v1.Admin/SubmitJob"}
	handler := func(ctx context.Context, _ any) (any, error) {
		return invokeSubmitJob(srv.(*Service), ctx, &req)
	}
	return interceptor(ctx, &req, info, handler)
}

// Migration shim; see the note on invokeGetCallers.
func invokeSubmitJob(svc *Service, ctx context.Context, req *SubmitJobRequest) (any, error) {
	pbResp, err := svc.SubmitJob(ctx, submitJobRequestToPB(req))
	if err != nil {
		return nil, err
	}
	resp := &SubmitJobResponse{JobID: pbResp.GetJobId()}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return &jsonMsg{Raw: raw}, nil
}

func handleGetJobStatus(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(jsonMsg)
	if err := dec(in); err != nil {
		return nil, err
	}
	var req GetJobStatusRequest
	if err := json.Unmarshal(in.Raw, &req); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return invokeGetJobStatus(srv.(*Service), ctx, &req)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/atlantis.admin.v1.Admin/GetJobStatus"}
	handler := func(ctx context.Context, _ any) (any, error) {
		return invokeGetJobStatus(srv.(*Service), ctx, &req)
	}
	return interceptor(ctx, &req, info, handler)
}

// Migration shim; see the note on invokeGetCallers.
func invokeGetJobStatus(svc *Service, ctx context.Context, req *GetJobStatusRequest) (any, error) {
	pbResp, err := svc.GetJobStatus(ctx, &adminpb.GetJobStatusRequest{JobId: req.JobID})
	if err != nil {
		return nil, err
	}
	// Job is a value on the JSON side, so a not-found response has always
	// carried a fully zero object rather than omitting the key. jobStatusFromPB
	// maps the nil message back to that zero value.
	resp := &GetJobStatusResponse{Found: pbResp.GetFound(), Job: jobStatusFromPB(pbResp.GetJob())}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return &jsonMsg{Raw: raw}, nil
}

func handleListDeadJobs(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(jsonMsg)
	if err := dec(in); err != nil {
		return nil, err
	}
	var req ListDeadJobsRequest
	if len(in.Raw) > 0 {
		if err := json.Unmarshal(in.Raw, &req); err != nil {
			return nil, err
		}
	}
	if interceptor == nil {
		return invokeListDeadJobs(srv.(*Service), ctx, &req)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/atlantis.admin.v1.Admin/ListDeadJobs"}
	handler := func(ctx context.Context, _ any) (any, error) {
		return invokeListDeadJobs(srv.(*Service), ctx, &req)
	}
	return interceptor(ctx, &req, info, handler)
}

// deadJobsToJSON returns nil for an empty list so the field marshals as null.
//
// Unlike ListConnectedWorkers, which emitted [] because the dispatcher handed
// it a non-nil slice, this handler built `var out []JobStatus` and only ever
// appended — so an empty dead-letter queue has always produced {"Jobs":null},
// and `tide job dead --format=json` prints that. There is no general rule
// here: each RPC has to match whatever its own pre-migration code emitted.
func deadJobsToJSON(in []*adminpb.JobStatus) []JobStatus {
	if len(in) == 0 {
		return nil
	}
	out := make([]JobStatus, 0, len(in))
	for _, j := range in {
		out = append(out, jobStatusFromPB(j))
	}
	return out
}

// Migration shim; see the note on invokeGetCallers.
func invokeListDeadJobs(svc *Service, ctx context.Context, req *ListDeadJobsRequest) (any, error) {
	// Clamp rather than narrow: an int64 limit above int32 wraps negative,
	// which this handler reads as "unset" and silently replaces with 50 —
	// turning an absurd request into a plausible-looking small answer.
	limit := req.Limit
	if limit > math.MaxInt32 {
		limit = math.MaxInt32
	}
	pbResp, err := svc.ListDeadJobs(ctx, &adminpb.ListDeadJobsRequest{
		JobName: req.JobName,
		Limit:   int32(limit),
	})
	if err != nil {
		return nil, err
	}
	resp := &ListDeadJobsResponse{Jobs: deadJobsToJSON(pbResp.GetJobs())}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return &jsonMsg{Raw: raw}, nil
}

func handleRetryDeadJob(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(jsonMsg)
	if err := dec(in); err != nil {
		return nil, err
	}
	var req RetryDeadJobRequest
	if err := json.Unmarshal(in.Raw, &req); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return invokeRetryDeadJob(srv.(*Service), ctx, &req)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/atlantis.admin.v1.Admin/RetryDeadJob"}
	handler := func(ctx context.Context, _ any) (any, error) {
		return invokeRetryDeadJob(srv.(*Service), ctx, &req)
	}
	return interceptor(ctx, &req, info, handler)
}

// Migration shim; see the note on invokeGetCallers.
func invokeRetryDeadJob(svc *Service, ctx context.Context, req *RetryDeadJobRequest) (any, error) {
	pbResp, err := svc.RetryDeadJob(ctx, &adminpb.RetryDeadJobRequest{JobId: req.JobID})
	if err != nil {
		return nil, err
	}
	resp := &RetryDeadJobResponse{JobID: pbResp.GetJobId()}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return &jsonMsg{Raw: raw}, nil
}

func handleStartWorkflow(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(jsonMsg)
	if err := dec(in); err != nil {
		return nil, err
	}
	var req StartWorkflowRequest
	if err := json.Unmarshal(in.Raw, &req); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return invokeStartWorkflow(srv.(*Service), ctx, &req)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/atlantis.admin.v1.Admin/StartWorkflow"}
	handler := func(ctx context.Context, _ any) (any, error) {
		return invokeStartWorkflow(srv.(*Service), ctx, &req)
	}
	return interceptor(ctx, &req, info, handler)
}

// Request mappings are extracted for the same reason the response ones are:
// the shims' early-return paths leave these fields unread, so a dropped field
// is invisible to any test that only exercises validation errors. Deleting
// State here would silently start every workflow with an empty state.
func startWorkflowRequestToPB(req *StartWorkflowRequest) *adminpb.StartWorkflowRequest {
	return &adminpb.StartWorkflowRequest{
		WorkflowName: req.WorkflowName,
		State:        req.State,
		SubmittedBy:  req.SubmittedBy,
	}
}

func getWorkflowStatusRequestToPB(req *GetWorkflowStatusRequest) *adminpb.GetWorkflowStatusRequest {
	return &adminpb.GetWorkflowStatusRequest{WorkflowId: req.WorkflowID}
}

func submitJobRequestToPB(req *SubmitJobRequest) *adminpb.SubmitJobRequest {
	return &adminpb.SubmitJobRequest{
		JobName:     req.JobName,
		Args:        req.Args,
		ScheduledAt: req.ScheduledAt,
		SubmittedBy: req.SubmittedBy,
	}
}

// Migration shim; see the note on invokeGetCallers.
func invokeStartWorkflow(svc *Service, ctx context.Context, req *StartWorkflowRequest) (any, error) {
	pbResp, err := svc.StartWorkflow(ctx, startWorkflowRequestToPB(req))
	if err != nil {
		return nil, err
	}
	resp := &StartWorkflowResponse{WorkflowID: pbResp.GetWorkflowId()}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return &jsonMsg{Raw: raw}, nil
}

func handleGetWorkflowStatus(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(jsonMsg)
	if err := dec(in); err != nil {
		return nil, err
	}
	var req GetWorkflowStatusRequest
	if err := json.Unmarshal(in.Raw, &req); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return invokeGetWorkflowStatus(srv.(*Service), ctx, &req)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/atlantis.admin.v1.Admin/GetWorkflowStatus"}
	handler := func(ctx context.Context, _ any) (any, error) {
		return invokeGetWorkflowStatus(srv.(*Service), ctx, &req)
	}
	return interceptor(ctx, &req, info, handler)
}

// Migration shim; see the note on invokeGetCallers.
func invokeGetWorkflowStatus(svc *Service, ctx context.Context, req *GetWorkflowStatusRequest) (any, error) {
	pbResp, err := svc.GetWorkflowStatus(ctx, getWorkflowStatusRequestToPB(req))
	if err != nil {
		return nil, err
	}
	// Workflow is a value on the JSON side, so a not-found response has always
	// carried a fully zero object rather than omitting the key.
	resp := &GetWorkflowStatusResponse{Found: pbResp.GetFound(), Workflow: workflowStatusFromPB(pbResp.GetWorkflow())}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return &jsonMsg{Raw: raw}, nil
}

// --- Schema versioning RPCs ---

func handleGetSchemaHistory(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(jsonMsg)
	if err := dec(in); err != nil {
		return nil, err
	}
	var req GetSchemaHistoryRequest
	if len(in.Raw) > 0 {
		if err := json.Unmarshal(in.Raw, &req); err != nil {
			return nil, err
		}
	}
	if interceptor == nil {
		return invokeGetSchemaHistory(srv.(*Service), ctx, &req)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/atlantis.admin.v1.Admin/GetSchemaHistory"}
	handler := func(ctx context.Context, _ any) (any, error) {
		return invokeGetSchemaHistory(srv.(*Service), ctx, &req)
	}
	return interceptor(ctx, &req, info, handler)
}

// Migration shim; see the note on invokeGetCallers.
func invokeGetSchemaHistory(svc *Service, ctx context.Context, req *GetSchemaHistoryRequest) (any, error) {
	pbResp, err := svc.GetSchemaHistory(ctx, &adminpb.GetSchemaHistoryRequest{Limit: req.Limit, Before: req.Before, Caller: req.Caller})
	if err != nil {
		return nil, err
	}
	resp := &GetSchemaHistoryResponse{Versions: schemaVersionSummariesFromPB(pbResp.GetVersions()), HasMore: pbResp.GetHasMore()}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return &jsonMsg{Raw: raw}, nil
}

func handleGetSchemaVersion(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(jsonMsg)
	if err := dec(in); err != nil {
		return nil, err
	}
	var req GetSchemaVersionRequest
	if err := json.Unmarshal(in.Raw, &req); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return invokeGetSchemaVersion(srv.(*Service), ctx, &req)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/atlantis.admin.v1.Admin/GetSchemaVersion"}
	handler := func(ctx context.Context, _ any) (any, error) {
		return invokeGetSchemaVersion(srv.(*Service), ctx, &req)
	}
	return interceptor(ctx, &req, info, handler)
}

// Migration shim; see the note on invokeGetCallers.
func invokeGetSchemaVersion(svc *Service, ctx context.Context, req *GetSchemaVersionRequest) (any, error) {
	pbResp, err := svc.GetSchemaVersion(ctx, &adminpb.GetSchemaVersionRequest{Version: req.Version})
	if err != nil {
		return nil, err
	}
	resp := schemaVersionFromPB(pbResp)
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return &jsonMsg{Raw: raw}, nil
}

func handleDiffSchemaVersions(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(jsonMsg)
	if err := dec(in); err != nil {
		return nil, err
	}
	var req DiffSchemaVersionsRequest
	if err := json.Unmarshal(in.Raw, &req); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return invokeDiffSchemaVersions(srv.(*Service), ctx, &req)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/atlantis.admin.v1.Admin/DiffSchemaVersions"}
	handler := func(ctx context.Context, _ any) (any, error) {
		return invokeDiffSchemaVersions(srv.(*Service), ctx, &req)
	}
	return interceptor(ctx, &req, info, handler)
}

// Migration shim; see the note on invokeGetCallers.
func invokeDiffSchemaVersions(svc *Service, ctx context.Context, req *DiffSchemaVersionsRequest) (any, error) {
	pbResp, err := svc.DiffSchemaVersions(ctx, &adminpb.DiffSchemaVersionsRequest{FromVersion: req.FromVersion, ToVersion: req.ToVersion})
	if err != nil {
		return nil, err
	}
	resp := &DiffSchemaVersionsResponse{FromVersion: pbResp.GetFromVersion(), ToVersion: pbResp.GetToVersion(), Diff: pbResp.GetDiff(), FromIR: pbResp.GetFromIr(), ToIR: pbResp.GetToIr()}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return &jsonMsg{Raw: raw}, nil
}

func handleGetEntityLineage(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(jsonMsg)
	if err := dec(in); err != nil {
		return nil, err
	}
	var req GetEntityLineageRequest
	if err := json.Unmarshal(in.Raw, &req); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return invokeGetEntityLineage(srv.(*Service), ctx, &req)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/atlantis.admin.v1.Admin/GetEntityLineage"}
	handler := func(ctx context.Context, _ any) (any, error) {
		return invokeGetEntityLineage(srv.(*Service), ctx, &req)
	}
	return interceptor(ctx, &req, info, handler)
}

// Migration shim; see the note on invokeGetCallers.
func invokeGetEntityLineage(svc *Service, ctx context.Context, req *GetEntityLineageRequest) (any, error) {
	pbResp, err := svc.GetEntityLineage(ctx, &adminpb.GetEntityLineageRequest{EntityId: req.EntityID})
	if err != nil {
		return nil, err
	}
	resp := &GetEntityLineageResponse{Entries: lineageFromPB(pbResp.GetEntries())}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return &jsonMsg{Raw: raw}, nil
}

func handleGetEntityOwners(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(jsonMsg)
	if err := dec(in); err != nil {
		return nil, err
	}
	var req GetEntityOwnersRequest
	if len(in.Raw) > 0 {
		if err := json.Unmarshal(in.Raw, &req); err != nil {
			return nil, err
		}
	}
	if interceptor == nil {
		return invokeGetEntityOwners(srv.(*Service), ctx, &req)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/atlantis.admin.v1.Admin/GetEntityOwners"}
	handler := func(ctx context.Context, _ any) (any, error) {
		return invokeGetEntityOwners(srv.(*Service), ctx, &req)
	}
	return interceptor(ctx, &req, info, handler)
}

// Migration shim; see the note on invokeGetCallers.
func invokeGetEntityOwners(svc *Service, ctx context.Context, req *GetEntityOwnersRequest) (any, error) {
	pbResp, err := svc.GetEntityOwners(ctx, &adminpb.GetEntityOwnersRequest{})
	if err != nil {
		return nil, err
	}
	resp := &GetEntityOwnersResponse{Owners: ownersFromPB(pbResp.GetOwners())}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return &jsonMsg{Raw: raw}, nil
}

func handleRollbackSchema(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(jsonMsg)
	if err := dec(in); err != nil {
		return nil, err
	}
	var req RollbackSchemaRequest
	if err := json.Unmarshal(in.Raw, &req); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return invokeRollbackSchema(srv.(*Service), ctx, &req)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/atlantis.admin.v1.Admin/RollbackSchema"}
	handler := func(ctx context.Context, _ any) (any, error) {
		return invokeRollbackSchema(srv.(*Service), ctx, &req)
	}
	return interceptor(ctx, &req, info, handler)
}

// Migration shim; see the note on invokeGetCallers.
func invokeRollbackSchema(svc *Service, ctx context.Context, req *RollbackSchemaRequest) (any, error) {
	pbResp, err := svc.RollbackSchema(ctx, &adminpb.RollbackSchemaRequest{ToVersion: req.ToVersion, Caller: req.Caller})
	if err != nil {
		return nil, err
	}
	resp := &RollbackSchemaResponse{NewVersion: pbResp.GetNewVersion(), UpSQL: pbResp.GetUpSql()}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return &jsonMsg{Raw: raw}, nil
}

func handlePreviewRollback(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(jsonMsg)
	if err := dec(in); err != nil {
		return nil, err
	}
	var req PreviewRollbackRequest
	if err := json.Unmarshal(in.Raw, &req); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return invokePreviewRollback(srv.(*Service), ctx, &req)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/atlantis.admin.v1.Admin/PreviewRollback"}
	handler := func(ctx context.Context, _ any) (any, error) {
		return invokePreviewRollback(srv.(*Service), ctx, &req)
	}
	return interceptor(ctx, &req, info, handler)
}

// Migration shim; see the note on invokeGetCallers.
func invokePreviewRollback(svc *Service, ctx context.Context, req *PreviewRollbackRequest) (any, error) {
	pbResp, err := svc.PreviewRollback(ctx, &adminpb.PreviewRollbackRequest{ToVersion: req.ToVersion})
	if err != nil {
		return nil, err
	}
	resp := &PreviewRollbackResponse{TargetVersion: pbResp.GetTargetVersion(), CurrentVersion: pbResp.GetCurrentVersion(), UpSQL: pbResp.GetUpSql(), PlanClass: pbResp.GetPlanClass(), ChangeCount: int(pbResp.GetChangeCount())}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return &jsonMsg{Raw: raw}, nil
}

func handleGetCallerFiles(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(jsonMsg)
	if err := dec(in); err != nil {
		return nil, err
	}
	var req GetCallerFilesRequest
	if err := json.Unmarshal(in.Raw, &req); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return invokeGetCallerFiles(srv.(*Service), ctx, &req)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/atlantis.admin.v1.Admin/GetCallerFiles"}
	handler := func(ctx context.Context, _ any) (any, error) {
		return invokeGetCallerFiles(srv.(*Service), ctx, &req)
	}
	return interceptor(ctx, &req, info, handler)
}

// Migration shim; see the note on invokeGetCallers.
func invokeGetCallerFiles(svc *Service, ctx context.Context, req *GetCallerFilesRequest) (any, error) {
	pbResp, err := svc.GetCallerFiles(ctx, &adminpb.GetCallerFilesRequest{Caller: req.Caller})
	if err != nil {
		return nil, err
	}
	resp := &GetCallerFilesResponse{Files: submittedFilesFromPB(pbResp.GetFiles())}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return &jsonMsg{Raw: raw}, nil
}

func handleGetCallers(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(jsonMsg)
	if err := dec(in); err != nil {
		return nil, err
	}
	var req GetCallersRequest
	if interceptor == nil {
		return invokeGetCallers(srv.(*Service), ctx, &req)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/atlantis.admin.v1.Admin/GetCallers"}
	handler := func(ctx context.Context, _ any) (any, error) {
		return invokeGetCallers(srv.(*Service), ctx, &req)
	}
	return interceptor(ctx, &req, info, handler)
}

// callerInfosToJSON rebuilds the pre-migration JSON shape from protobuf.
//
// Extracted so it can be tested directly: CallerInfo carries omitempty on
// three fields, so which keys appear in the emitted object depends on zero
// values surviving the round trip exactly. That is a hand-maintained
// invariant, and the console SPA reads the result — the kind of thing a later
// cleanup deletes because it looks redundant.
//
// The nil return for an empty input is deliberate and load-bearing: the
// pre-migration handler built `var out []CallerInfo` and never assigned it
// when there were no rows, so the response marshalled as `"callers":null`.
// Returning an empty slice would emit `"callers":[]` instead.
func callerInfosToJSON(in []*adminpb.CallerInfo) []CallerInfo {
	if len(in) == 0 {
		return nil
	}
	out := make([]CallerInfo, 0, len(in))
	for _, c := range in {
		out = append(out, CallerInfo{
			Caller:        c.GetCaller(),
			FileCount:     int(c.GetFileCount()),
			LastAppliedAt: c.GetLastAppliedAt(),
			SchemaVersion: c.GetSchemaVersion(),
			Registered:    c.GetRegistered(),
			CanMutate:     c.GetCanMutate(),
			CertExpiresAt: c.GetCertExpiresAt(),
		})
	}
	return out
}

// Migration shim: the Service method now takes and returns protobuf types, so
// these handlers translate for as long as the JSON path exists. Both they and
// the structs they decode into disappear with this file.
func invokeGetCallers(svc *Service, ctx context.Context, _ *GetCallersRequest) (any, error) {
	pbResp, err := svc.GetCallers(ctx, &adminpb.GetCallersRequest{})
	if err != nil {
		return nil, err
	}
	resp := &GetCallersResponse{Callers: callerInfosToJSON(pbResp.GetCallers())}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return &jsonMsg{Raw: raw}, nil
}

func handleRegisterCaller(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(jsonMsg)
	if err := dec(in); err != nil {
		return nil, err
	}
	var req RegisterCallerRequest
	if err := json.Unmarshal(in.Raw, &req); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return invokeRegisterCaller(srv.(*Service), ctx, &req)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/atlantis.admin.v1.Admin/RegisterCaller"}
	handler := func(ctx context.Context, _ any) (any, error) {
		return invokeRegisterCaller(srv.(*Service), ctx, &req)
	}
	return interceptor(ctx, &req, info, handler)
}

// Migration shim; see invokeGetCallerAliases.
func invokeRegisterCaller(svc *Service, ctx context.Context, req *RegisterCallerRequest) (any, error) {
	pbResp, err := svc.RegisterCaller(ctx, &adminpb.RegisterCallerRequest{
		Caller:    req.Caller,
		CanMutate: req.CanMutate,
		CreatedBy: req.CreatedBy,
	})
	if err != nil {
		return nil, err
	}
	resp := &RegisterCallerResponse{Caller: pbResp.GetCaller(), CanMutate: pbResp.GetCanMutate()}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return &jsonMsg{Raw: raw}, nil
}

func handleRevokeCaller(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(jsonMsg)
	if err := dec(in); err != nil {
		return nil, err
	}
	var req RevokeCallerRequest
	if err := json.Unmarshal(in.Raw, &req); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return invokeRevokeCaller(srv.(*Service), ctx, &req)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/atlantis.admin.v1.Admin/RevokeCaller"}
	handler := func(ctx context.Context, _ any) (any, error) {
		return invokeRevokeCaller(srv.(*Service), ctx, &req)
	}
	return interceptor(ctx, &req, info, handler)
}

// Migration shim; see invokeGetCallerAliases.
func invokeRevokeCaller(svc *Service, ctx context.Context, req *RevokeCallerRequest) (any, error) {
	pbResp, err := svc.RevokeCaller(ctx, &adminpb.RevokeCallerRequest{Caller: req.Caller})
	if err != nil {
		return nil, err
	}
	resp := &RevokeCallerResponse{FilesRemoved: int(pbResp.GetFilesRemoved())}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return &jsonMsg{Raw: raw}, nil
}

func handleRecordCallerCertExpiry(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(jsonMsg)
	if err := dec(in); err != nil {
		return nil, err
	}
	var req RecordCallerCertExpiryRequest
	if err := json.Unmarshal(in.Raw, &req); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return invokeRecordCallerCertExpiry(srv.(*Service), ctx, &req)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/atlantis.admin.v1.Admin/RecordCallerCertExpiry"}
	handler := func(ctx context.Context, _ any) (any, error) {
		return invokeRecordCallerCertExpiry(srv.(*Service), ctx, &req)
	}
	return interceptor(ctx, &req, info, handler)
}

// Migration shim; see invokeGetCallerAliases.
func invokeRecordCallerCertExpiry(svc *Service, ctx context.Context, req *RecordCallerCertExpiryRequest) (any, error) {
	pbResp, err := svc.RecordCallerCertExpiry(ctx, &adminpb.RecordCallerCertExpiryRequest{
		Caller:      req.Caller,
		ExpiresAt:   req.ExpiresAt,
		Fingerprint: req.Fingerprint,
	})
	if err != nil {
		return nil, err
	}
	resp := &RecordCallerCertExpiryResponse{Caller: pbResp.GetCaller(), ExpiresAt: pbResp.GetExpiresAt()}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return &jsonMsg{Raw: raw}, nil
}

func handleGetLogs(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(jsonMsg)
	if err := dec(in); err != nil {
		return nil, err
	}
	var req GetLogsRequest
	if err := json.Unmarshal(in.Raw, &req); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return invokeGetLogs(srv.(*Service), ctx, &req)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/atlantis.admin.v1.Admin/GetLogs"}
	handler := func(ctx context.Context, _ any) (any, error) {
		return invokeGetLogs(srv.(*Service), ctx, &req)
	}
	return interceptor(ctx, &req, info, handler)
}

// Migration shim; see the note on invokeGetCallers.
func invokeGetLogs(svc *Service, ctx context.Context, req *GetLogsRequest) (any, error) {
	// Clamp rather than narrow, as invokeListDeadJobs does: an int64 limit
	// above int32 wraps to a small positive number, so a request for "no
	// meaningful cap" silently returns one record. Reachable — the console's
	// /api/logs?limit= parses into an int and only rejects n <= 0.
	logLimit := req.Limit
	if logLimit > math.MaxInt32 {
		logLimit = math.MaxInt32
	}
	pbResp, err := svc.GetLogs(ctx, &adminpb.GetLogsRequest{Since: req.Since, Limit: int32(logLimit)})
	if err != nil {
		return nil, err
	}
	resp := &GetLogsResponse{Records: logEntriesFromPB(pbResp.GetRecords()), LastSeq: pbResp.GetLastSeq()}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return &jsonMsg{Raw: raw}, nil
}

// --- Worker dispatcher admin RPCs ---

func handleListConnectedWorkers(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(jsonMsg)
	if err := dec(in); err != nil {
		return nil, err
	}
	var req ListConnectedWorkersRequest
	if len(in.Raw) > 0 {
		_ = json.Unmarshal(in.Raw, &req)
	}
	if interceptor == nil {
		return invokeListConnectedWorkers(srv.(*Service), ctx, &req)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/atlantis.admin.v1.Admin/ListConnectedWorkers"}
	handler := func(ctx context.Context, _ any) (any, error) {
		return invokeListConnectedWorkers(srv.(*Service), ctx, &req)
	}
	return interceptor(ctx, &req, info, handler)
}

// Migration shim; see the note on invokeGetCallers.
func invokeListConnectedWorkers(svc *Service, ctx context.Context, _ *ListConnectedWorkersRequest) (any, error) {
	pbResp, err := svc.ListConnectedWorkers(ctx, &adminpb.ListConnectedWorkersRequest{})
	if err != nil {
		return nil, err
	}
	// Non-nil so an empty result marshals as [] — see the note on empty
	// repeated fields in workers.go.
	sessions := make([]DispatcherSessionSnapshot, 0, len(pbResp.GetSessions()))
	for _, p := range pbResp.GetSessions() {
		sessions = append(sessions, sessionSnapshotFromPB(p))
	}
	resp := &ListConnectedWorkersResponse{Sessions: sessions}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return &jsonMsg{Raw: raw}, nil
}

func handleGetWorkerSession(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(jsonMsg)
	if err := dec(in); err != nil {
		return nil, err
	}
	var req GetWorkerSessionRequest
	if err := json.Unmarshal(in.Raw, &req); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return invokeGetWorkerSession(srv.(*Service), ctx, &req)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/atlantis.admin.v1.Admin/GetWorkerSession"}
	handler := func(ctx context.Context, _ any) (any, error) {
		return invokeGetWorkerSession(srv.(*Service), ctx, &req)
	}
	return interceptor(ctx, &req, info, handler)
}

// Migration shim; see the note on invokeGetCallers.
func invokeGetWorkerSession(svc *Service, ctx context.Context, req *GetWorkerSessionRequest) (any, error) {
	pbResp, err := svc.GetWorkerSession(ctx, &adminpb.GetWorkerSessionRequest{SessionId: req.SessionID})
	if err != nil {
		return nil, err
	}
	resp := &GetWorkerSessionResponse{Session: sessionDetailFromPB(pbResp.GetSession())}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return &jsonMsg{Raw: raw}, nil
}

func handleDrainWorker(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(jsonMsg)
	if err := dec(in); err != nil {
		return nil, err
	}
	var req DrainWorkerRequest
	if err := json.Unmarshal(in.Raw, &req); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return invokeDrainWorker(srv.(*Service), ctx, &req)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/atlantis.admin.v1.Admin/DrainWorker"}
	handler := func(ctx context.Context, _ any) (any, error) {
		return invokeDrainWorker(srv.(*Service), ctx, &req)
	}
	return interceptor(ctx, &req, info, handler)
}

// Migration shim; see the note on invokeGetCallers.
func invokeDrainWorker(svc *Service, ctx context.Context, req *DrainWorkerRequest) (any, error) {
	if _, err := svc.DrainWorker(ctx, &adminpb.DrainWorkerRequest{SessionId: req.SessionID}); err != nil {
		return nil, err
	}
	resp := &DrainWorkerResponse{}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return &jsonMsg{Raw: raw}, nil
}

func handleEvictWorker(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(jsonMsg)
	if err := dec(in); err != nil {
		return nil, err
	}
	var req EvictWorkerRequest
	if err := json.Unmarshal(in.Raw, &req); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return invokeEvictWorker(srv.(*Service), ctx, &req)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/atlantis.admin.v1.Admin/EvictWorker"}
	handler := func(ctx context.Context, _ any) (any, error) {
		return invokeEvictWorker(srv.(*Service), ctx, &req)
	}
	return interceptor(ctx, &req, info, handler)
}

// Migration shim; see the note on invokeGetCallers.
func invokeEvictWorker(svc *Service, ctx context.Context, req *EvictWorkerRequest) (any, error) {
	if _, err := svc.EvictWorker(ctx, &adminpb.EvictWorkerRequest{SessionId: req.SessionID}); err != nil {
		return nil, err
	}
	resp := &EvictWorkerResponse{}
	raw, err := json.Marshal(resp)
	if err != nil {
		return nil, err
	}
	return &jsonMsg{Raw: raw}, nil
}

// --- Caller alias RPCs ---

func handleGetCallerAliases(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(jsonMsg)
	if err := dec(in); err != nil {
		return nil, err
	}
	var req GetCallerAliasesRequest
	if err := json.Unmarshal(in.Raw, &req); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return invokeGetCallerAliases(srv.(*Service), ctx, &req)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/atlantis.admin.v1.Admin/GetCallerAliases"}
	handler := func(ctx context.Context, _ any) (any, error) {
		return invokeGetCallerAliases(srv.(*Service), ctx, &req)
	}
	return interceptor(ctx, &req, info, handler)
}

// Migration shim: GetCallerAliases now takes and returns protobuf types, so
// this handler translates for as long as the JSON path exists. Both this
// function and the struct it decodes into disappear with the file.
func invokeGetCallerAliases(svc *Service, ctx context.Context, req *GetCallerAliasesRequest) (any, error) {
	resp, err := svc.GetCallerAliases(ctx, &adminpb.GetCallerAliasesRequest{Caller: req.Caller})
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(GetCallerAliasesResponse{
		Caller:  resp.GetCaller(),
		Aliases: resp.GetAliases(),
	})
	if err != nil {
		return nil, err
	}
	return &jsonMsg{Raw: raw}, nil
}

func handleSetCallerAliases(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(jsonMsg)
	if err := dec(in); err != nil {
		return nil, err
	}
	var req SetCallerAliasesRequest
	if err := json.Unmarshal(in.Raw, &req); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return invokeSetCallerAliases(srv.(*Service), ctx, &req)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/atlantis.admin.v1.Admin/SetCallerAliases"}
	handler := func(ctx context.Context, _ any) (any, error) {
		return invokeSetCallerAliases(srv.(*Service), ctx, &req)
	}
	return interceptor(ctx, &req, info, handler)
}

// Migration shim; see invokeGetCallerAliases.
func invokeSetCallerAliases(svc *Service, ctx context.Context, req *SetCallerAliasesRequest) (any, error) {
	resp, err := svc.SetCallerAliases(ctx, &adminpb.SetCallerAliasesRequest{
		Caller:  req.Caller,
		Aliases: req.Aliases,
	})
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(SetCallerAliasesResponse{
		Caller:  resp.GetCaller(),
		Aliases: resp.GetAliases(),
	})
	if err != nil {
		return nil, err
	}
	return &jsonMsg{Raw: raw}, nil
}
