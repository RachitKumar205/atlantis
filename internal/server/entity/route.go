package entity

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// methodHandler is the shape of grpc.MethodDesc.Handler.
type methodHandler func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error)

// serviceRoute is one service the snapshot serves, for reflection.
type serviceRoute struct {
	// metadata is the proto path the code generator emits for the service,
	// the value grpc.ServiceDesc.Metadata carries for a compiled service.
	metadata string
	methods  []string
}

// Load builds the first snapshot from ir. The services it declares are
// served through Handler from the next request on.
func (s *Server) Load(ir *dsl.IR) error {
	if ir == nil {
		return fmt.Errorf("entity.Load: nil IR")
	}
	snap, err := buildSnapshot(ir, "")
	if err != nil {
		return err
	}
	return s.install(snap)
}

// install builds the route table for snap and makes it current. The
// handlers close over s.
//
// Custom queries and procedures share one CustomService per namespace, as
// the emitted proto declares them; its descriptor is built here from the
// per-query files so reflection can describe it.
func (s *Server) install(snap *entitySnapshot) error {
	snap.routes = make(map[string]methodHandler)
	snap.services = make(map[string]*serviceRoute)

	for _, meta := range snap.entities {
		ns := goNamespace(meta.entity.Namespace)
		name := meta.entity.Name
		svc := fmt.Sprintf("atlantis.%s.v1.%sService", ns, name)
		sr := &serviceRoute{metadata: entityDynamicFilePath(ns, name)}
		for _, op := range []string{"Get", "Create", "Update", "Delete", "BatchGet", "Query"} {
			m := op + name
			sr.methods = append(sr.methods, m)
			snap.routes["/"+svc+"/"+m] = makeHandler(s, meta.entityID, op, ns, name)
		}
		snap.services[svc] = sr
	}

	type customMethod struct {
		name    string
		file    protoreflect.FileDescriptor
		handler methodHandler
	}
	byNS := make(map[string][]customMethod)
	for key, cqm := range snap.customMeta {
		ns := splitEntityID(cqm.query.Owner)[0]
		byNS[ns] = append(byNS[ns], customMethod{cqm.query.Name, cqm.requestDesc.ParentFile(), makeCustomHandler(s, key, ns)})
	}
	for key, pm := range snap.procMeta {
		ns := splitEntityID(pm.proc.Owner)[0]
		byNS[ns] = append(byNS[ns], customMethod{pm.proc.Name, pm.requestDesc.ParentFile(), makeCustomProcedureHandler(s, key, ns)})
	}
	for ns, methods := range byNS {
		goNS := goNamespace(ns)
		pkg := fmt.Sprintf("atlantis.%s.v1", goNS)
		svc := pkg + ".CustomService"
		sort.Slice(methods, func(i, j int) bool { return methods[i].name < methods[j].name })

		file := &descriptorpb.FileDescriptorProto{
			Name:    strPtr(fmt.Sprintf("atlantis/%s/v1/custom_dynamic.proto", goNS)),
			Package: strPtr(pkg),
			Syntax:  strPtr("proto3"),
		}
		svcDesc := &descriptorpb.ServiceDescriptorProto{Name: strPtr("CustomService")}
		sr := &serviceRoute{metadata: file.GetName()}
		for _, m := range methods {
			file.Dependency = append(file.Dependency, m.file.Path())
			svcDesc.Method = append(svcDesc.Method, &descriptorpb.MethodDescriptorProto{
				Name:       strPtr(m.name),
				InputType:  strPtr("." + pkg + "." + m.name + "Request"),
				OutputType: strPtr("." + pkg + "." + m.name + "Response"),
			})
			sr.methods = append(sr.methods, m.name)
			snap.routes["/"+svc+"/"+m.name] = m.handler
		}
		file.Service = append(file.Service, svcDesc)
		fd, err := buildFileDescriptor(file, snap.files)
		if err != nil {
			return fmt.Errorf("building CustomService descriptor for %s: %w", ns, err)
		}
		snap.files[fd.Path()] = fd
		snap.services[svc] = sr
	}
	for _, sr := range snap.services {
		sort.Strings(sr.methods)
	}

	s.snapshot.Store(snap)
	return nil
}

// Serves reports whether the current snapshot routes fullMethod.
func (s *Server) Serves(fullMethod string) bool {
	_, ok := s.snapshot.Load().routes[fullMethod]
	return ok
}

// Handler returns the grpc.UnknownServiceHandler that serves every entity,
// custom-query and procedure RPC from the current snapshot, so a reload adds
// or removes a service on a running server. grpc.Server refuses
// RegisterService once Serve has been called.
//
// Every routed RPC is unary. unary runs around it, since the server's own
// unary chain does not reach an unknown-service handler; the stream chain
// must skip these methods (see interceptors.StreamChainUnless) or each call
// is intercepted twice.
//
// grpc.Server also sends an unknown method of a service it registered to
// this handler; knownStatic reports whether it registered the named service,
// so the refusal names the method.
//
// Serves and Handler may read different snapshots across a reload. A method
// the reload adds between the two runs both chains once; one it removes
// returns Unimplemented with neither.
func (s *Server) Handler(unary grpc.UnaryServerInterceptor, knownStatic func(service string) bool) grpc.StreamHandler {
	return func(srv any, ss grpc.ServerStream) error {
		fullMethod, ok := grpc.MethodFromServerStream(ss)
		if !ok {
			return status.Error(codes.Internal, "no method name on stream")
		}
		snap := s.snapshot.Load()
		h, ok := snap.routes[fullMethod]
		if !ok {
			return unimplemented(snap, fullMethod, knownStatic)
		}
		resp, err := h(srv, ss.Context(), ss.RecvMsg, unary)
		if err != nil {
			return err
		}
		return ss.SendMsg(resp)
	}
}

// unimplemented returns the status grpc.Server itself returns for a method
// it does not serve, so a client sees the same error either way.
func unimplemented(snap *entitySnapshot, fullMethod string, knownStatic func(string) bool) error {
	pos := strings.LastIndex(fullMethod, "/")
	if pos == -1 || fullMethod[0] != '/' {
		return status.Errorf(codes.Unimplemented, "malformed method name: %q", fullMethod)
	}
	service, method := fullMethod[1:pos], fullMethod[pos+1:]
	if _, ok := snap.services[service]; ok || (knownStatic != nil && knownStatic(service)) {
		return status.Errorf(codes.Unimplemented, "unknown method %v for service %v", method, service)
	}
	return status.Errorf(codes.Unimplemented, "unknown service %v", service)
}

// GetServiceInfo lists the services the current snapshot serves, in the
// form grpc.Server.GetServiceInfo uses, for server reflection.
func (s *Server) GetServiceInfo() map[string]grpc.ServiceInfo {
	snap := s.snapshot.Load()
	out := make(map[string]grpc.ServiceInfo, len(snap.services))
	for name, sr := range snap.services {
		info := grpc.ServiceInfo{Metadata: sr.metadata}
		for _, m := range sr.methods {
			info.Methods = append(info.Methods, grpc.MethodInfo{Name: m})
		}
		out[name] = info
	}
	return out
}

// ServiceInfoWith returns a provider listing static's services and the
// current snapshot's together, for reflection.ServerOptions.Services.
func (s *Server) ServiceInfoWith(static interface {
	GetServiceInfo() map[string]grpc.ServiceInfo
}) interface {
	GetServiceInfo() map[string]grpc.ServiceInfo
} {
	return serviceInfoUnion{static: static, dynamic: s}
}

type serviceInfoUnion struct {
	static, dynamic interface {
		GetServiceInfo() map[string]grpc.ServiceInfo
	}
}

func (u serviceInfoUnion) GetServiceInfo() map[string]grpc.ServiceInfo {
	out := u.static.GetServiceInfo()
	for name, info := range u.dynamic.GetServiceInfo() {
		out[name] = info
	}
	return out
}

// FindFileByPath resolves a file the current snapshot built, then falls back
// to the global registry. With FindDescriptorByName it makes Server a
// protodesc.Resolver for reflection.ServerOptions.DescriptorResolver.
func (s *Server) FindFileByPath(path string) (protoreflect.FileDescriptor, error) {
	if fd, ok := s.snapshot.Load().files[path]; ok {
		return fd, nil
	}
	return protoregistry.GlobalFiles.FindFileByPath(path)
}

// FindDescriptorByName resolves a descriptor the current snapshot built, then
// falls back to the global registry.
func (s *Server) FindDescriptorByName(name protoreflect.FullName) (protoreflect.Descriptor, error) {
	for _, fd := range s.snapshot.Load().files {
		if d := findInFile(fd, name); d != nil {
			return d, nil
		}
	}
	return protoregistry.GlobalFiles.FindDescriptorByName(name)
}
