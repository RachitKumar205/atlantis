package entity

import (
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/rachitkumar205/atlantis/internal/cache/queryresult"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/runtime"
)

// Server is the dynamic entity server that reads the DSL IR at startup
// and serves every entity's CRUD RPCs without compiled proto stubs.
type Server struct {
	pool       runtime.Pool
	cache      runtime.Cache
	outbox     runtime.Outbox
	queryCache *queryresult.Cache
	entities   map[string]*entityMeta
}

// NewServer constructs a dynamic entity server.
func NewServer(pool runtime.Pool, cache runtime.Cache, outbox runtime.Outbox, qc *queryresult.Cache) *Server {
	return &Server{
		pool:       pool,
		cache:      cache,
		outbox:     outbox,
		queryCache: qc,
		entities:   make(map[string]*entityMeta),
	}
}

// Register reads the IR and registers one gRPC service per entity
// (Get, Create, Update, Delete, BatchGet, Query) plus per-namespace
// CustomService descriptors for custom queries.
func (s *Server) Register(grpcSrv *grpc.Server, ir *dsl.IR) error {
	if ir == nil {
		return fmt.Errorf("entity.Register: nil IR")
	}

	for i := range ir.Entities {
		e := &ir.Entities[i]
		meta := buildEntityMeta(e, ir)

		fd, err := buildProtoDescriptors(e)
		if err != nil {
			return fmt.Errorf("entity %s: %w", e.ID(), err)
		}
		resolveProtoDescriptors(meta, fd)

		// Sanity: every critical descriptor must be present.
		if meta.msgDesc == nil {
			return fmt.Errorf("entity %s: entity message descriptor not built", e.ID())
		}
		if meta.getRequestDesc == nil {
			return fmt.Errorf("entity %s: GetRequest descriptor not built", e.ID())
		}

		s.entities[e.ID()] = meta
	}

	// Register one gRPC service per entity.
	for _, meta := range s.entities {
		desc := buildGRPCServiceDesc(s, meta)
		grpcSrv.RegisterService(&desc, s)
	}

	// Register custom query services (one per namespace).
	if len(ir.Queries) > 0 {
		if err := s.registerCustomServices(grpcSrv, ir); err != nil {
			return err
		}
	}

	return nil
}

// buildGRPCServiceDesc constructs the grpc.ServiceDesc for one entity.
// It uses the same service name as the compiled proto stubs so callers
// (which send compiled proto messages) connect to the same endpoints.
func buildGRPCServiceDesc(s *Server, meta *entityMeta) grpc.ServiceDesc {
	ns := goNamespace(meta.entity.Namespace)
	serviceName := fmt.Sprintf("atlantis.%s.v1.%sService", ns, meta.entity.Name)

	methods := []grpc.MethodDesc{
		{MethodName: "Get" + meta.entity.Name, Handler: makeHandler(s, meta, "Get")},
		{MethodName: "Create" + meta.entity.Name, Handler: makeHandler(s, meta, "Create")},
		{MethodName: "Update" + meta.entity.Name, Handler: makeHandler(s, meta, "Update")},
		{MethodName: "Delete" + meta.entity.Name, Handler: makeHandler(s, meta, "Delete")},
		{MethodName: "BatchGet" + meta.entity.Name, Handler: makeHandler(s, meta, "BatchGet")},
		{MethodName: "Query" + meta.entity.Name, Handler: makeHandler(s, meta, "Query")},
	}

	return grpc.ServiceDesc{
		ServiceName: serviceName,
		HandlerType: nil, // dynamic; no typed interface to check
		Methods:     methods,
		Streams:     []grpc.StreamDesc{},
		Metadata:    fmt.Sprintf("atlantis/%s/v1/%s.proto", ns, meta.entity.Name),
	}
}

func (s *Server) registerCustomServices(grpcSrv *grpc.Server, ir *dsl.IR) error {
	type nsGroup struct {
		ns      string
		methods []grpc.MethodDesc
	}
	groups := make(map[string]*nsGroup)

	for i := range ir.Queries {
		cq := &ir.Queries[i]
		parts := splitEntityID(cq.Owner)
		ns := parts[0]

		cqm := &customQueryMeta{
			query:     cq,
			sql:       cq.SQL,
			inputCols: cq.Inputs,
			timeoutMS: 2000,
		}

		if cq.Output.AsEntityID != "" {
			cqm.asEntity = true
			if em, ok := s.entities[cq.Output.AsEntityID]; ok {
				cqm.entityMeta = em
			}
		} else {
			cqm.outputCols = cq.Output.Columns
		}

		// Build proto descriptors for this custom query.
		fd, err := buildCustomQueryDescs(cq, ns)
		if err != nil {
			return fmt.Errorf("custom query %s: %w", cq.Name, err)
		}

		cqm.requestDesc = fd.Messages().ByName(protoreflect.Name(cq.Name + "Request"))
		cqm.responseDesc = fd.Messages().ByName(protoreflect.Name(cq.Name + "Response"))

		// For column-output queries, find the nested Row message.
		if len(cq.Output.Columns) > 0 && cqm.responseDesc != nil {
			rowName := protoreflect.Name(cq.Name + "Response_Row")
			cqm.rowDesc = cqm.responseDesc.Messages().ByName(rowName)
		}

		g, ok := groups[ns]
		if !ok {
			g = &nsGroup{ns: ns}
			groups[ns] = g
		}
		g.methods = append(g.methods, grpc.MethodDesc{
			MethodName: cq.Name,
			Handler:    makeCustomHandler(s, cqm, ns),
		})
	}

	for _, g := range groups {
		goNS := goNamespace(g.ns)
		desc := grpc.ServiceDesc{
			ServiceName: fmt.Sprintf("atlantis.%s.v1.CustomService", goNS),
			HandlerType: nil,
			Methods:     g.methods,
			Streams:     []grpc.StreamDesc{},
			Metadata:    fmt.Sprintf("atlantis/%s/v1/custom.proto", goNS),
		}
		grpcSrv.RegisterService(&desc, s)
	}

	return nil
}
