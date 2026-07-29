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

// jsonCodec mirrors the codec the `tide` CLI installs on the client side.
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
	PlanSchema(context.Context, PlanRequest) (*PlanResponse, error)
	ApplyMigration(context.Context, ApplyRequest) (*ApplyResponse, error)
	GetMergedSchema(context.Context, GetMergedSchemaRequest) (*GetMergedSchemaResponse, error)
	GetCanonicalIR(context.Context, GetCanonicalIRRequest) (*GetCanonicalIRResponse, error)
	BeginBackfillPlan(context.Context, BeginBackfillPlanRequest) (*BeginBackfillPlanResponse, error)
	GetBackfillStatus(context.Context, GetBackfillStatusRequest) (*GetBackfillStatusResponse, error)
	AdoptBaseline(context.Context, AdoptBaselineRequest) (*AdoptBaselineResponse, error)
	SubmitJob(context.Context, *adminpb.SubmitJobRequest) (*adminpb.SubmitJobResponse, error)
	GetJobStatus(context.Context, *adminpb.GetJobStatusRequest) (*adminpb.GetJobStatusResponse, error)
	ListDeadJobs(context.Context, *adminpb.ListDeadJobsRequest) (*adminpb.ListDeadJobsResponse, error)
	RetryDeadJob(context.Context, *adminpb.RetryDeadJobRequest) (*adminpb.RetryDeadJobResponse, error)
	StartWorkflow(context.Context, StartWorkflowRequest) (*StartWorkflowResponse, error)
	GetWorkflowStatus(context.Context, GetWorkflowStatusRequest) (*GetWorkflowStatusResponse, error)
	GetSchemaHistory(context.Context, GetSchemaHistoryRequest) (*GetSchemaHistoryResponse, error)
	GetSchemaVersion(context.Context, GetSchemaVersionRequest) (*GetSchemaVersionResponse, error)
	DiffSchemaVersions(context.Context, DiffSchemaVersionsRequest) (*DiffSchemaVersionsResponse, error)
	GetEntityLineage(context.Context, GetEntityLineageRequest) (*GetEntityLineageResponse, error)
	GetEntityOwners(context.Context, GetEntityOwnersRequest) (*GetEntityOwnersResponse, error)
	RollbackSchema(context.Context, RollbackSchemaRequest) (*RollbackSchemaResponse, error)
	PreviewRollback(context.Context, PreviewRollbackRequest) (*PreviewRollbackResponse, error)
	GetCallerFiles(context.Context, GetCallerFilesRequest) (*GetCallerFilesResponse, error)
	GetCallers(context.Context, *adminpb.GetCallersRequest) (*adminpb.GetCallersResponse, error)
	RegisterCaller(context.Context, *adminpb.RegisterCallerRequest) (*adminpb.RegisterCallerResponse, error)
	RevokeCaller(context.Context, *adminpb.RevokeCallerRequest) (*adminpb.RevokeCallerResponse, error)
	RecordCallerCertExpiry(context.Context, *adminpb.RecordCallerCertExpiryRequest) (*adminpb.RecordCallerCertExpiryResponse, error)
	GetLogs(context.Context, GetLogsRequest) (*GetLogsResponse, error)
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

func invokePlan(svc *Service, ctx context.Context, req *PlanRequest) (any, error) {
	resp, err := svc.PlanSchema(ctx, *req)
	if err != nil {
		return nil, err
	}
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

func invokeApply(svc *Service, ctx context.Context, req *ApplyRequest) (any, error) {
	resp, err := svc.ApplyMigration(ctx, *req)
	if err != nil {
		return nil, err
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

func invokeGetMergedSchema(svc *Service, ctx context.Context, req *GetMergedSchemaRequest) (any, error) {
	resp, err := svc.GetMergedSchema(ctx, *req)
	if err != nil {
		return nil, err
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

func invokeGetCanonicalIR(svc *Service, ctx context.Context, req *GetCanonicalIRRequest) (any, error) {
	resp, err := svc.GetCanonicalIR(ctx, *req)
	if err != nil {
		return nil, err
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

func invokeBeginBackfillPlan(svc *Service, ctx context.Context, req *BeginBackfillPlanRequest) (any, error) {
	resp, err := svc.BeginBackfillPlan(ctx, *req)
	if err != nil {
		return nil, err
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

func invokeGetBackfillStatus(svc *Service, ctx context.Context, req *GetBackfillStatusRequest) (any, error) {
	resp, err := svc.GetBackfillStatus(ctx, *req)
	if err != nil {
		return nil, err
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

func invokeAdoptBaseline(svc *Service, ctx context.Context, req *AdoptBaselineRequest) (any, error) {
	resp, err := svc.AdoptBaseline(ctx, *req)
	if err != nil {
		return nil, err
	}
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
	pbResp, err := svc.SubmitJob(ctx, &adminpb.SubmitJobRequest{
		JobName:     req.JobName,
		Args:        req.Args,
		ScheduledAt: req.ScheduledAt,
		SubmittedBy: req.SubmittedBy,
	})
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

func invokeStartWorkflow(svc *Service, ctx context.Context, req *StartWorkflowRequest) (any, error) {
	resp, err := svc.StartWorkflow(ctx, *req)
	if err != nil {
		return nil, err
	}
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

func invokeGetWorkflowStatus(svc *Service, ctx context.Context, req *GetWorkflowStatusRequest) (any, error) {
	resp, err := svc.GetWorkflowStatus(ctx, *req)
	if err != nil {
		return nil, err
	}
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

func invokeGetSchemaHistory(svc *Service, ctx context.Context, req *GetSchemaHistoryRequest) (any, error) {
	resp, err := svc.GetSchemaHistory(ctx, *req)
	if err != nil {
		return nil, err
	}
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

func invokeGetSchemaVersion(svc *Service, ctx context.Context, req *GetSchemaVersionRequest) (any, error) {
	resp, err := svc.GetSchemaVersion(ctx, *req)
	if err != nil {
		return nil, err
	}
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

func invokeDiffSchemaVersions(svc *Service, ctx context.Context, req *DiffSchemaVersionsRequest) (any, error) {
	resp, err := svc.DiffSchemaVersions(ctx, *req)
	if err != nil {
		return nil, err
	}
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

func invokeGetEntityLineage(svc *Service, ctx context.Context, req *GetEntityLineageRequest) (any, error) {
	resp, err := svc.GetEntityLineage(ctx, *req)
	if err != nil {
		return nil, err
	}
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

func invokeGetEntityOwners(svc *Service, ctx context.Context, req *GetEntityOwnersRequest) (any, error) {
	resp, err := svc.GetEntityOwners(ctx, *req)
	if err != nil {
		return nil, err
	}
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

func invokeRollbackSchema(svc *Service, ctx context.Context, req *RollbackSchemaRequest) (any, error) {
	resp, err := svc.RollbackSchema(ctx, *req)
	if err != nil {
		return nil, err
	}
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

func invokePreviewRollback(svc *Service, ctx context.Context, req *PreviewRollbackRequest) (any, error) {
	resp, err := svc.PreviewRollback(ctx, *req)
	if err != nil {
		return nil, err
	}
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

func invokeGetCallerFiles(svc *Service, ctx context.Context, req *GetCallerFilesRequest) (any, error) {
	resp, err := svc.GetCallerFiles(ctx, *req)
	if err != nil {
		return nil, err
	}
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

func invokeGetLogs(svc *Service, ctx context.Context, req *GetLogsRequest) (any, error) {
	resp, err := svc.GetLogs(ctx, *req)
	if err != nil {
		return nil, err
	}
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
