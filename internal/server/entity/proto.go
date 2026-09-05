package entity

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/rachitkumar205/atlantis/internal/coltype"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/schema"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	// QueryXRequest.fields is a google.protobuf.FieldMask, resolved through
	// protoregistry.GlobalFiles. Nothing else here links the package, so
	// without this import the descriptor fails to build with "file not
	// found: google/protobuf/field_mask.proto".
	_ "google.golang.org/protobuf/types/known/fieldmaskpb"
)

// buildProtoDescriptors constructs protoreflect descriptors for an entity
// at runtime using descriptorpb. The resulting message descriptors use
// the same field numbers the codegen assigns (Field.ProtoNumber), so
// callers sending compiled proto messages produce identical wire bytes.
//
// includeRefs are the foreign keys pointing at this entity, in the order
// schema.InboundRefs returns them — the same order codegen numbers the
// <Entity>Include enum from.
//
// A field the emitted .proto declares and this builder omits is not a
// compatible subset: it arrives as an unknown field and is discarded without
// an error, so the request the caller wrote and the request the server serves
// differ silently. TestDynamicDescriptorMatchesEmittedProto holds the two
// declarations together.
//
// Returns the file descriptor containing all messages and services for this entity.
func buildProtoDescriptors(e *dsl.Entity, includeRefs []schema.InboundRef) (protoreflect.FileDescriptor, error) {
	// A field left at 0 collides with every other field left at 0, and
	// protodesc reports that as "conflicting fields" naming two columns that
	// do not conflict. The fault is a checkpoint written without
	// AssignProtoNumbers, so it is named here as that.
	for i := range e.Fields {
		if e.Fields[i].ProtoNumber == 0 {
			return nil, fmt.Errorf("field %q has no proto number: this checkpoint was written without AssignProtoNumbers", e.Fields[i].Name)
		}
	}

	ns := goNamespace(e.Namespace)
	pkg := fmt.Sprintf("atlantis.%s.v1", ns)
	fileName := fmt.Sprintf("atlantis/%s/v1/%s_dynamic.proto", ns, schema.SnakeCase(e.Name))

	file := &descriptorpb.FileDescriptorProto{
		Name:    strPtr(fileName),
		Package: strPtr(pkg),
		Syntax:  strPtr("proto3"),
	}

	// The files declaring the message types this entity's columns resolve to.
	//
	// Read out of the same coltype.ProtoType that decides the field's type,
	// so a column cannot name a message the descriptor does not import. Asking
	// the type class instead covered timestamps and missed `interval`, whose
	// wire form is atlantis.common.v1.Interval: protodesc refused the whole
	// file with "cannot resolve type", so an entity declaring one interval
	// column took every entity in the schema down with it at startup.
	colTypes := make([]dsl.FieldType, 0, len(e.Fields))
	for _, f := range e.Fields {
		colTypes = append(colTypes, f.Type)
	}

	// Entity message.
	entityMsg := buildEntityMessage(e)
	file.MessageType = append(file.MessageType, entityMsg)

	// PK columns for request shells.
	pkCols := schema.PKColumns(e)
	composite := len(pkCols) > 1

	// Composite PK wrapper message.
	if composite {
		pkMsg := buildPKMessage(e, pkCols)
		file.MessageType = append(file.MessageType, pkMsg)
	}

	// Request/Response messages.
	file.MessageType = append(file.MessageType,
		buildGetRequest(e, pkCols),
		buildGetResponse(e),
		buildCreateRequest(e),
		buildCreateResponse(e),
		buildUpdateRequest(e),
		buildUpdateResponse(e),
		buildDeleteRequest(e, pkCols),
		buildDeleteResponse(e),
		buildBatchGetRequest(e, pkCols, composite),
		buildBatchGetResponse(e),
		buildQueryRequest(e),
		buildQueryResponse(e),
	)

	// Filter message.
	filterMsg := buildFilterMessage(e)
	file.MessageType = append(file.MessageType, filterMsg)

	// The query surface's two enums and the message wrapping one of them.
	file.EnumType = append(file.EnumType,
		buildOrderFieldEnum(e),
		buildIncludeEnum(e, includeRefs),
	)
	file.MessageType = append(file.MessageType, buildOrderByMessage(e))

	// Service definition.
	svc := buildServiceDescriptor(e)
	file.Service = append(file.Service, svc)

	file.Dependency = append(file.Dependency, protoDependenciesFor(colTypes)...)
	// The filter message references predicates from
	// atlantis/common/v1/predicates.proto (unless no field is filterable,
	// which is degenerate) and QueryXRequest.fields is a FieldMask. Both are
	// added unconditionally — protodesc tolerates an unused dependency.
	file.Dependency = append(file.Dependency,
		"atlantis/common/v1/predicates.proto",
		"google/protobuf/field_mask.proto")

	fd, err := buildFileDescriptor(file)
	if err != nil {
		return nil, fmt.Errorf("building file descriptor for %s: %w", e.ID(), err)
	}

	// Sanity check: the entity message and filter message must be present.
	entityDesc := fd.Messages().ByName(protoreflect.Name(e.Name))
	if entityDesc == nil {
		return nil, fmt.Errorf("entity message %s not found in built descriptor", e.Name)
	}
	filterName := protoreflect.Name(e.Name + "Filter")
	filterDescr := fd.Messages().ByName(filterName)
	if filterDescr == nil {
		return nil, fmt.Errorf("filter message %s not found in built descriptor", filterName)
	}

	return fd, nil
}

func buildEntityMessage(e *dsl.Entity) *descriptorpb.DescriptorProto {
	msg := &descriptorpb.DescriptorProto{
		Name: strPtr(e.Name),
	}

	for _, f := range e.Fields {
		field := dslFieldToProtoField(&f)
		msg.Field = append(msg.Field, field)
	}

	// Reserved numbers for retired proto fields.
	if len(e.RetiredProtoNumbers) > 0 {
		for _, n := range e.RetiredProtoNumbers {
			n32 := int32(n)
			msg.ReservedRange = append(msg.ReservedRange, &descriptorpb.DescriptorProto_ReservedRange{
				Start: &n32,
				End:   int32Ptr(n32 + 1), // end is exclusive
			})
		}
	}

	return msg
}

// dslFieldToProtoField converts a DSL field to a proto FieldDescriptorProto
// using the same type mapping as coltype.ProtoType.
func dslFieldToProtoField(f *dsl.Field) *descriptorpb.FieldDescriptorProto {
	num := int32(f.ProtoNumber)
	fd := &descriptorpb.FieldDescriptorProto{
		Name:   strPtr(f.Name),
		Number: &num,
	}

	applyProtoFieldType(fd, f.Type)
	// Nullable scalars are proto3-optional for presence tracking; repeated
	// fields (array/vector) never take proto3-optional. Setting the flag is
	// only half of it — materializeProto3Optional supplies the synthetic
	// oneof that actually carries the presence.
	if !f.Type.Array && f.Type.Name != "vector" && schema.IsEffectivelyNullable(f) {
		fd.Proto3Optional = boolPtr(true)
	}
	return fd
}

// buildFileDescriptor is the only way this package turns a
// FileDescriptorProto into a live descriptor.
//
// It exists so materializeProto3Optional cannot be skipped. There are three
// descriptor builders here — entities, custom queries, custom procedures — and
// a fix applied at one call site is a fix that the next builder, or the next
// message added to an existing one, silently does not get. Only the entity
// builder marks fields proto3-optional today; routing every builder through
// here is what stops that from mattering.
func buildFileDescriptor(file *descriptorpb.FileDescriptorProto) (protoreflect.FileDescriptor, error) {
	for _, msg := range file.MessageType {
		materializeProto3Optional(msg)
	}
	// The resolver chains files built here with the global proto registry,
	// which holds the compiled Timestamp, predicates and so on from
	// init()-time registration.
	resolver := &fileResolver{
		files:  make(map[string]protoreflect.FileDescriptor),
		global: protoregistry.GlobalFiles,
	}
	return protodesc.NewFile(file, resolver)
}

// materializeProto3Optional gives every proto3-optional field in msg, and in
// every message nested inside it, the single-field synthetic oneof that proto3
// presence is built on.
//
// protodesc.NewFile does not do this. protoc synthesizes these oneofs when it
// compiles a .proto and writes them into the FileDescriptorProto it emits;
// protodesc consumes that descriptor as given. It validates the pairing — a
// proto3-optional field inside a oneof must be its only member — and does not
// create it, so a descriptor built in Go with the flag set and no oneof passes
// validation and yields a field with no presence.
//
// Without this, every nullable scalar column served by the dynamic dispatcher
// has HasPresence() == false and Has() degrades to "differs from the zero
// value". Two paths depend on it:
//
//   - bindColumnValue reads Has() to choose between binding a value and binding
//     SQL NULL, so a client explicitly sending `count = 0` or `note = ""` on a
//     nullable column writes NULL.
//   - protoValueForCursor cannot tell a NULL ordering column from a real zero.
//
// A test that leaves a field unset gets the right answer either way; only an
// explicit zero separates the two implementations.
//
// protoc names the oneof `_<field>` and prepends further underscores on
// collision; this mirrors that so a descriptor built here and one compiled from
// the equivalent .proto agree. Synthetic oneofs must also follow every real
// one, which holds trivially here — no message this package builds declares a
// real oneof — but the append order is what keeps it true if one ever does.
func materializeProto3Optional(msg *descriptorpb.DescriptorProto) {
	// Nested first, so a message added inside another one is covered without
	// the caller knowing it is there. The custom-query builder nests its row
	// message inside the response.
	for _, nested := range msg.NestedType {
		materializeProto3Optional(nested)
	}
	taken := make(map[string]bool, len(msg.OneofDecl))
	for _, od := range msg.OneofDecl {
		taken[od.GetName()] = true
	}
	for _, f := range msg.Field {
		if !f.GetProto3Optional() || f.OneofIndex != nil {
			continue
		}
		name := "_" + f.GetName()
		for taken[name] {
			name = "_" + name
		}
		taken[name] = true
		idx := int32(len(msg.OneofDecl))
		msg.OneofDecl = append(msg.OneofDecl, &descriptorpb.OneofDescriptorProto{
			Name: strPtr(name),
		})
		f.OneofIndex = &idx
	}
}

// applyProtoFieldType sets a field descriptor's Type and Label from a DSL
// field type: vector(N) and []T are LABEL_REPEATED (repeated float /
// repeated <elem>), every other type is a LABEL_OPTIONAL scalar. This is
// the single place that decides a field's proto cardinality, shared by the
// entity and custom-query descriptor builders so the two can't diverge.
// They did once: the custom-query builder hard-coded LABEL_OPTIONAL and
// left vector inputs as a scalar float, so the client's packed 768-float
// payload hit a wire-type mismatch (skipped → 0) and the runtime
// dispatcher panicked ("cannot convert float32 to list") once it tried to
// read the field as a list.
func applyProtoFieldType(fd *descriptorpb.FieldDescriptorProto, t dsl.FieldType) {
	if t.Array {
		rep := descriptorpb.FieldDescriptorProto_LABEL_REPEATED
		fd.Label = &rep
		if t.Elem != nil {
			setProtoType(fd, *t.Elem)
		} else {
			// bare array with no elem info — default to string
			typ := descriptorpb.FieldDescriptorProto_TYPE_STRING
			fd.Type = &typ
		}
		return
	}
	if t.Name == "vector" {
		rep := descriptorpb.FieldDescriptorProto_LABEL_REPEATED
		fd.Label = &rep
		typ := descriptorpb.FieldDescriptorProto_TYPE_FLOAT
		fd.Type = &typ
		return
	}
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	fd.Label = &opt
	setProtoType(fd, t)
}

// setProtoType publishes the descriptor field type for a column.
//
// It reads coltype.ProtoType, which is the same function `tide codegen` emits
// from, so a generated client and this dispatcher describe a column the same
// way. A second switch here diverges silently: a client putting a message on
// the wire against a descriptor declaring a string unmarshals without error and
// leaves the field unset, so the value vanishes between the caller and
// Postgres.
//
// An unmapped type falls to string, which is what a column of no known type
// stringifies to on the read path.
func setProtoType(fd *descriptorpb.FieldDescriptorProto, t dsl.FieldType) {
	set := func(k descriptorpb.FieldDescriptorProto_Type) {
		fd.Type = &k
	}
	pt, err := coltype.ProtoType(t)
	if err != nil {
		set(descriptorpb.FieldDescriptorProto_TYPE_STRING)
		return
	}
	switch pt {
	case "int32":
		set(descriptorpb.FieldDescriptorProto_TYPE_INT32)
	case "int64":
		set(descriptorpb.FieldDescriptorProto_TYPE_INT64)
	case "float", "repeated float":
		set(descriptorpb.FieldDescriptorProto_TYPE_FLOAT)
	case "double":
		set(descriptorpb.FieldDescriptorProto_TYPE_DOUBLE)
	case "string":
		set(descriptorpb.FieldDescriptorProto_TYPE_STRING)
	case "bool":
		set(descriptorpb.FieldDescriptorProto_TYPE_BOOL)
	case "bytes":
		set(descriptorpb.FieldDescriptorProto_TYPE_BYTES)
	case "google.protobuf.Timestamp":
		set(descriptorpb.FieldDescriptorProto_TYPE_MESSAGE)
		fd.TypeName = strPtr(".google.protobuf.Timestamp")
	case "atlantis.common.v1.Interval":
		set(descriptorpb.FieldDescriptorProto_TYPE_MESSAGE)
		fd.TypeName = strPtr(".atlantis.common.v1.Interval")
	default:
		set(descriptorpb.FieldDescriptorProto_TYPE_STRING)
	}
}

// protoDependencyFor names the file declaring a column's wire type, and ""
// for a scalar, which needs no import.
//
// Keyed on coltype.ProtoType so it answers for exactly the message types
// setProtoType resolves. TestEveryDocumentedTypeBuildsADescriptor builds one
// entity per documented type and holds the two switches to the same set.
func protoDependencyFor(t dsl.FieldType) string {
	pt, err := coltype.ProtoType(t)
	if err != nil {
		return ""
	}
	switch pt {
	case "google.protobuf.Timestamp":
		return "google/protobuf/timestamp.proto"
	case "atlantis.common.v1.Interval":
		return "atlantis/common/v1/interval.proto"
	}
	return ""
}

// protoDependenciesFor names the files these columns' wire types are declared
// in, deduplicated and sorted.
//
// All three descriptor builders here call it. Each previously carried its own
// test — `Name == "timestamptz" || Name == "date"` — which named two of the
// three types coltype maps to a message and none of the aliases: an `interval`
// column failed to resolve and protodesc refused the whole file.
func protoDependenciesFor(types []dsl.FieldType) []string {
	deps := map[string]bool{}
	for _, t := range types {
		if dep := protoDependencyFor(t); dep != "" {
			deps[dep] = true
		}
	}
	return slices.Sorted(maps.Keys(deps))
}

func buildPKMessage(e *dsl.Entity, pkCols []*dsl.Field) *descriptorpb.DescriptorProto {
	msg := &descriptorpb.DescriptorProto{
		Name: strPtr(e.Name + "PK"),
	}
	for i, f := range pkCols {
		num := int32(i + 1)
		fd := &descriptorpb.FieldDescriptorProto{
			Name:   strPtr(f.Name),
			Number: &num,
		}
		label := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
		fd.Label = &label
		setProtoType(fd, f.Type)
		msg.Field = append(msg.Field, fd)
	}
	return msg
}

func buildGetRequest(e *dsl.Entity, pkCols []*dsl.Field) *descriptorpb.DescriptorProto {
	msg := &descriptorpb.DescriptorProto{
		Name: strPtr("Get" + e.Name + "Request"),
	}
	for i, f := range pkCols {
		num := int32(i + 1)
		fd := &descriptorpb.FieldDescriptorProto{
			Name:   strPtr(f.Name),
			Number: &num,
		}
		label := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
		fd.Label = &label
		setProtoType(fd, f.Type)
		msg.Field = append(msg.Field, fd)
	}
	return msg
}

func buildGetResponse(e *dsl.Entity) *descriptorpb.DescriptorProto {
	return wrapEntityResponse("Get"+e.Name+"Response", e)
}

func buildCreateRequest(e *dsl.Entity) *descriptorpb.DescriptorProto {
	return wrapEntityRequest("Create"+e.Name+"Request", e)
}

func buildCreateResponse(e *dsl.Entity) *descriptorpb.DescriptorProto {
	return wrapEntityResponse("Create"+e.Name+"Response", e)
}

func buildUpdateRequest(e *dsl.Entity) *descriptorpb.DescriptorProto {
	return wrapEntityRequest("Update"+e.Name+"Request", e)
}

func buildUpdateResponse(e *dsl.Entity) *descriptorpb.DescriptorProto {
	return wrapEntityResponse("Update"+e.Name+"Response", e)
}

func buildDeleteRequest(e *dsl.Entity, pkCols []*dsl.Field) *descriptorpb.DescriptorProto {
	msg := &descriptorpb.DescriptorProto{
		Name: strPtr("Delete" + e.Name + "Request"),
	}
	for i, f := range pkCols {
		num := int32(i + 1)
		fd := &descriptorpb.FieldDescriptorProto{
			Name:   strPtr(f.Name),
			Number: &num,
		}
		label := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
		fd.Label = &label
		setProtoType(fd, f.Type)
		msg.Field = append(msg.Field, fd)
	}
	return msg
}

func buildDeleteResponse(e *dsl.Entity) *descriptorpb.DescriptorProto {
	return &descriptorpb.DescriptorProto{
		Name: strPtr("Delete" + e.Name + "Response"),
	}
}

func buildBatchGetRequest(e *dsl.Entity, pkCols []*dsl.Field, composite bool) *descriptorpb.DescriptorProto {
	msg := &descriptorpb.DescriptorProto{
		Name: strPtr("BatchGet" + e.Name + "Request"),
	}
	one := int32(1)
	label := descriptorpb.FieldDescriptorProto_LABEL_REPEATED

	if composite {
		// repeated <Entity>PK ids = 1;
		ns := goNamespace(e.Namespace)
		typeName := fmt.Sprintf(".atlantis.%s.v1.%sPK", ns, e.Name)
		typ := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
		msg.Field = append(msg.Field, &descriptorpb.FieldDescriptorProto{
			Name:     strPtr("ids"),
			Number:   &one,
			Label:    &label,
			Type:     &typ,
			TypeName: strPtr(typeName),
		})
	} else {
		// repeated <pk_type> <pk_name>s = 1;
		fd := &descriptorpb.FieldDescriptorProto{
			Name:   strPtr(pkCols[0].Name + "s"),
			Number: &one,
			Label:  &label,
		}
		setProtoType(fd, pkCols[0].Type)
		msg.Field = append(msg.Field, fd)
	}
	return msg
}

func buildBatchGetResponse(e *dsl.Entity) *descriptorpb.DescriptorProto {
	return wrapRepeatedEntityResponse("BatchGet"+e.Name+"Response", e)
}

// orderFields returns the entity's fields that may sit in an ORDER BY, in
// proto-number order — the order the <Entity>OrderField enum numbers them in.
//
// Declaration order is not proto-number order once a field has been added
// after a retirement, so the sort is what keeps this enum and the emitted one
// listing the same variants.
func orderFields(e *dsl.Entity) []*dsl.Field {
	out := make([]*dsl.Field, 0, len(e.Fields))
	for i := range e.Fields {
		if !coltype.Orderable(e.Fields[i].Type) {
			continue
		}
		out = append(out, &e.Fields[i])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProtoNumber < out[j].ProtoNumber })
	return out
}

// buildOrderFieldEnum builds <Entity>OrderField: UNSPECIFIED at 0, then one
// variant per orderable field carrying that field's proto number.
//
// Sharing the numbering with the entity message is what lets buildKeysetCols
// resolve a variant to a column through the existing column metadata rather
// than through a second table.
func buildOrderFieldEnum(e *dsl.Entity) *descriptorpb.EnumDescriptorProto {
	name := e.Name + "OrderField"
	prefix := schema.ScreamingSnake(name)
	zero := int32(0)
	enum := &descriptorpb.EnumDescriptorProto{
		Name: strPtr(name),
		Value: []*descriptorpb.EnumValueDescriptorProto{
			{Name: strPtr(prefix + "_UNSPECIFIED"), Number: &zero},
		},
	}
	for _, f := range orderFields(e) {
		n := int32(f.ProtoNumber)
		enum.Value = append(enum.Value, &descriptorpb.EnumValueDescriptorProto{
			Name:   strPtr(prefix + "_" + strings.ToUpper(f.Name)),
			Number: &n,
		})
	}
	return enum
}

// buildOrderByMessage builds <Entity>OrderBy: which column, and which way.
func buildOrderByMessage(e *dsl.Entity) *descriptorpb.DescriptorProto {
	ns := goNamespace(e.Namespace)
	one, two := int32(1), int32(2)
	enumType := descriptorpb.FieldDescriptorProto_TYPE_ENUM
	boolType := descriptorpb.FieldDescriptorProto_TYPE_BOOL
	optLabel := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	return &descriptorpb.DescriptorProto{
		Name: strPtr(e.Name + "OrderBy"),
		Field: []*descriptorpb.FieldDescriptorProto{
			{
				Name:     strPtr("field"),
				Number:   &one,
				Label:    &optLabel,
				Type:     &enumType,
				TypeName: strPtr(fmt.Sprintf(".atlantis.%s.v1.%sOrderField", ns, e.Name)),
			},
			{
				Name:   strPtr("desc"),
				Number: &two,
				Label:  &optLabel,
				Type:   &boolType,
			},
		},
	}
}

// buildIncludeEnum builds <Entity>Include: UNSPECIFIED at 0, then one variant
// per foreign key pointing at this entity, numbered by position.
//
// The dispatcher serves no include, so every variant past UNSPECIFIED names
// something handleQuery refuses. They are declared anyway: an undeclared
// repeated field is dropped as unknown, which is the difference between a
// caller learning includes are unimplemented and a caller receiving rows with
// the relation silently absent.
func buildIncludeEnum(e *dsl.Entity, refs []schema.InboundRef) *descriptorpb.EnumDescriptorProto {
	name := e.Name + "Include"
	prefix := schema.ScreamingSnake(name)
	zero := int32(0)
	enum := &descriptorpb.EnumDescriptorProto{
		Name: strPtr(name),
		Value: []*descriptorpb.EnumValueDescriptorProto{
			{Name: strPtr(prefix + "_UNSPECIFIED"), Number: &zero},
		},
	}
	for i, ref := range refs {
		ns, ent, ok := strings.Cut(ref.FromEntityID, ".")
		if !ok {
			ns, ent = "", ref.FromEntityID
		}
		n := int32(i + 1)
		enum.Value = append(enum.Value, &descriptorpb.EnumValueDescriptorProto{
			Name: strPtr(fmt.Sprintf("%s_%s_%s_BY_%s",
				prefix,
				schema.ScreamingSnake(goNamespace(ns)),
				schema.ScreamingSnake(ent),
				strings.ToUpper(ref.FromField))),
			Number: &n,
		})
	}
	return enum
}

func buildQueryRequest(e *dsl.Entity) *descriptorpb.DescriptorProto {
	msg := &descriptorpb.DescriptorProto{
		Name: strPtr("Query" + e.Name + "Request"),
	}
	ns := goNamespace(e.Namespace)
	filterTypeName := fmt.Sprintf(".atlantis.%s.v1.%sFilter", ns, e.Name)

	one := int32(1)
	two := int32(2)
	three := int32(3)
	four := int32(4)
	five := int32(5)
	six := int32(6)
	seven := int32(7)

	msgType := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
	enumType := descriptorpb.FieldDescriptorProto_TYPE_ENUM
	int32Type := descriptorpb.FieldDescriptorProto_TYPE_INT32
	stringType := descriptorpb.FieldDescriptorProto_TYPE_STRING
	boolType := descriptorpb.FieldDescriptorProto_TYPE_BOOL
	optLabel := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	repLabel := descriptorpb.FieldDescriptorProto_LABEL_REPEATED

	msg.Field = append(msg.Field,
		&descriptorpb.FieldDescriptorProto{
			Name:     strPtr("filter"),
			Number:   &one,
			Label:    &optLabel,
			Type:     &msgType,
			TypeName: strPtr(filterTypeName),
		},
		&descriptorpb.FieldDescriptorProto{
			Name:     strPtr("order"),
			Number:   &two,
			Label:    &repLabel,
			Type:     &msgType,
			TypeName: strPtr(fmt.Sprintf(".atlantis.%s.v1.%sOrderBy", ns, e.Name)),
		},
		&descriptorpb.FieldDescriptorProto{
			Name:   strPtr("limit"),
			Number: &three,
			Label:  &optLabel,
			Type:   &int32Type,
		},
		&descriptorpb.FieldDescriptorProto{
			Name:   strPtr("page_token"),
			Number: &four,
			Label:  &optLabel,
			Type:   &stringType,
		},
		&descriptorpb.FieldDescriptorProto{
			Name:     strPtr("fields"),
			Number:   &five,
			Label:    &optLabel,
			Type:     &msgType,
			TypeName: strPtr(".google.protobuf.FieldMask"),
		},
		&descriptorpb.FieldDescriptorProto{
			Name:     strPtr("includes"),
			Number:   &six,
			Label:    &repLabel,
			Type:     &enumType,
			TypeName: strPtr(fmt.Sprintf(".atlantis.%s.v1.%sInclude", ns, e.Name)),
		},
		&descriptorpb.FieldDescriptorProto{
			Name:   strPtr("cache_skip"),
			Number: &seven,
			Label:  &optLabel,
			Type:   &boolType,
		},
	)
	return msg
}

func buildQueryResponse(e *dsl.Entity) *descriptorpb.DescriptorProto {
	msg := &descriptorpb.DescriptorProto{
		Name: strPtr("Query" + e.Name + "Response"),
	}
	ns := goNamespace(e.Namespace)
	entityTypeName := fmt.Sprintf(".atlantis.%s.v1.%s", ns, e.Name)

	one := int32(1)
	two := int32(2)
	three := int32(3)
	msgType := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
	stringType := descriptorpb.FieldDescriptorProto_TYPE_STRING
	int64Type := descriptorpb.FieldDescriptorProto_TYPE_INT64
	repLabel := descriptorpb.FieldDescriptorProto_LABEL_REPEATED
	optLabel := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL

	msg.Field = append(msg.Field,
		&descriptorpb.FieldDescriptorProto{
			Name:     strPtr("entities"),
			Number:   &one,
			Label:    &repLabel,
			Type:     &msgType,
			TypeName: strPtr(entityTypeName),
		},
		&descriptorpb.FieldDescriptorProto{
			Name:   strPtr("next_page_token"),
			Number: &two,
			Label:  &optLabel,
			Type:   &stringType,
		},
		&descriptorpb.FieldDescriptorProto{
			Name:           strPtr("total_estimate"),
			Number:         &three,
			Label:          &optLabel,
			Type:           &int64Type,
			Proto3Optional: boolPtr(true),
		},
	)
	return msg
}

// buildFilterMessage creates the <Entity>Filter message. One field per
// filterable column, using the predicate message type naming convention.
func buildFilterMessage(e *dsl.Entity) *descriptorpb.DescriptorProto {
	msg := &descriptorpb.DescriptorProto{
		Name: strPtr(e.Name + "Filter"),
	}

	// A filter field carries the entity field's own proto number, which is
	// what the emitted .proto assigns it.
	//
	// Numbering these 1..n instead — one slot per filterable column, in order
	// — agrees only while every column is filterable and the numbers are
	// contiguous. A vector, interval or array column occupies a number and
	// takes no filter slot, so every filterable column after it shifts down by
	// one against the client's. Measured on id(1) / embedding(2) / title(3) /
	// note(4): the client's `title` predicate is field 3, the dispatcher reads
	// field 3 as `note`, both are StringPredicate so it decodes without error,
	// and the server filters a column the caller did not name.
	//
	// TestDynamicDescriptorMatchesEmittedProto compares the two numberings.
	fields := slices.Clone(e.Fields)
	sort.Slice(fields, func(i, j int) bool {
		return fields[i].ProtoNumber < fields[j].ProtoNumber
	})
	for _, f := range fields {
		predMsg, ok := predicateMessageForField(f.Type)
		if !ok {
			continue
		}
		msgType := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
		optLabel := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
		typeName := fmt.Sprintf(".atlantis.common.v1.%s", predMsg)
		n := int32(f.ProtoNumber)
		msg.Field = append(msg.Field, &descriptorpb.FieldDescriptorProto{
			Name:     strPtr(f.Name),
			Number:   &n,
			Label:    &optLabel,
			Type:     &msgType,
			TypeName: strPtr(typeName),
		})
	}

	// Composition arms: and, or, not (standard filter composition fields).
	ns := goNamespace(e.Namespace)
	selfTypeName := fmt.Sprintf(".atlantis.%s.v1.%sFilter", ns, e.Name)
	andNum := int32(100)
	orNum := int32(101)
	notNum := int32(102)
	repLabel := descriptorpb.FieldDescriptorProto_LABEL_REPEATED
	optLabel := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	msgType := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE

	msg.Field = append(msg.Field,
		&descriptorpb.FieldDescriptorProto{
			Name:     strPtr("and"),
			Number:   &andNum,
			Label:    &repLabel,
			Type:     &msgType,
			TypeName: strPtr(selfTypeName),
		},
		&descriptorpb.FieldDescriptorProto{
			Name:     strPtr("or"),
			Number:   &orNum,
			Label:    &repLabel,
			Type:     &msgType,
			TypeName: strPtr(selfTypeName),
		},
		&descriptorpb.FieldDescriptorProto{
			Name:     strPtr("not"),
			Number:   &notNum,
			Label:    &optLabel,
			Type:     &msgType,
			TypeName: strPtr(selfTypeName),
		},
	)

	return msg
}

func buildServiceDescriptor(e *dsl.Entity) *descriptorpb.ServiceDescriptorProto {
	ns := goNamespace(e.Namespace)
	pkg := fmt.Sprintf(".atlantis.%s.v1", ns)

	svc := &descriptorpb.ServiceDescriptorProto{
		Name: strPtr(e.Name + "Service"),
	}

	// Standard CRUD methods.
	methods := []struct {
		name   string
		input  string
		output string
	}{
		{"Get" + e.Name, "Get" + e.Name + "Request", "Get" + e.Name + "Response"},
		{"Create" + e.Name, "Create" + e.Name + "Request", "Create" + e.Name + "Response"},
		{"Update" + e.Name, "Update" + e.Name + "Request", "Update" + e.Name + "Response"},
		{"Delete" + e.Name, "Delete" + e.Name + "Request", "Delete" + e.Name + "Response"},
		{"BatchGet" + e.Name, "BatchGet" + e.Name + "Request", "BatchGet" + e.Name + "Response"},
		{"Query" + e.Name, "Query" + e.Name + "Request", "Query" + e.Name + "Response"},
	}

	for _, m := range methods {
		svc.Method = append(svc.Method, &descriptorpb.MethodDescriptorProto{
			Name:       strPtr(m.name),
			InputType:  strPtr(pkg + "." + m.input),
			OutputType: strPtr(pkg + "." + m.output),
		})
	}

	return svc
}

// predicateMessageForField maps a DSL field type to the predicate proto
// message name.
func predicateMessageForField(t dsl.FieldType) (string, bool) {
	stem, ok := coltype.PredicateStem(t)
	if !ok {
		return "", false
	}
	return stem + "Predicate", true
}

// wrapEntityRequest creates a message with a single entity field at number 1.
func wrapEntityRequest(name string, e *dsl.Entity) *descriptorpb.DescriptorProto {
	ns := goNamespace(e.Namespace)
	typeName := fmt.Sprintf(".atlantis.%s.v1.%s", ns, e.Name)
	one := int32(1)
	msgType := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
	optLabel := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	return &descriptorpb.DescriptorProto{
		Name: strPtr(name),
		Field: []*descriptorpb.FieldDescriptorProto{
			{
				Name:     strPtr("entity"),
				Number:   &one,
				Label:    &optLabel,
				Type:     &msgType,
				TypeName: strPtr(typeName),
			},
		},
	}
}

// wrapEntityResponse creates a message with a single entity field at number 1.
func wrapEntityResponse(name string, e *dsl.Entity) *descriptorpb.DescriptorProto {
	return wrapEntityRequest(name, e)
}

// wrapRepeatedEntityResponse creates a message with a repeated entity field at number 1.
func wrapRepeatedEntityResponse(name string, e *dsl.Entity) *descriptorpb.DescriptorProto {
	ns := goNamespace(e.Namespace)
	typeName := fmt.Sprintf(".atlantis.%s.v1.%s", ns, e.Name)
	one := int32(1)
	msgType := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
	repLabel := descriptorpb.FieldDescriptorProto_LABEL_REPEATED
	return &descriptorpb.DescriptorProto{
		Name: strPtr(name),
		Field: []*descriptorpb.FieldDescriptorProto{
			{
				Name:     strPtr("entities"),
				Number:   &one,
				Label:    &repLabel,
				Type:     &msgType,
				TypeName: strPtr(typeName),
			},
		},
	}
}

// goNamespace mirrors codegen/proto.go — maps "vendor" to "vendorpkg".
func goNamespace(ns string) string {
	if ns == "vendor" {
		return "vendorpkg"
	}
	return ns
}

// fileResolver implements protodesc.Resolver for dependency lookup.
// It chains a local map of built files with the global proto registry
// so that compiled well-known types (Timestamp) and the predicates
// proto are automatically available.
type fileResolver struct {
	files  map[string]protoreflect.FileDescriptor
	global *protoregistry.Files
}

func (r *fileResolver) FindFileByPath(path string) (protoreflect.FileDescriptor, error) {
	if fd, ok := r.files[path]; ok {
		return fd, nil
	}
	if r.global != nil {
		return r.global.FindFileByPath(path)
	}
	return nil, fmt.Errorf("file not found: %s", path)
}

func (r *fileResolver) FindDescriptorByName(name protoreflect.FullName) (protoreflect.Descriptor, error) {
	for _, fd := range r.files {
		if d := findInFile(fd, name); d != nil {
			return d, nil
		}
	}
	if r.global != nil {
		return r.global.FindDescriptorByName(name)
	}
	return nil, fmt.Errorf("descriptor not found: %s", name)
}

// findInFile searches a file descriptor for a named descriptor.
func findInFile(fd protoreflect.FileDescriptor, name protoreflect.FullName) protoreflect.Descriptor {
	msgs := fd.Messages()
	for i := range msgs.Len() {
		m := msgs.Get(i)
		if m.FullName() == name {
			return m
		}
	}
	svcs := fd.Services()
	for i := range svcs.Len() {
		s := svcs.Get(i)
		if s.FullName() == name {
			return s
		}
	}
	return nil
}

// Helper functions for creating pointers to proto primitive types.
func strPtr(s string) *string { return &s }
func int32Ptr(i int32) *int32 { return &i }
func boolPtr(b bool) *bool    { return &b }
