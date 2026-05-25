// Package entity provides runtime schema dispatch for atlantis entity
// CRUD. Instead of tens of thousands of lines of generated Go, this
// package reads the DSL IR at startup and serves every entity RPC
// dynamically.
package entity

import (
	"github.com/rachitkumar205/atlantis/internal/codegen/query"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/schema"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// entityMeta holds the pre-computed metadata that the generic CRUD
// handlers consult at request time. Built once per entity at startup
// (or hot-reload); never mutated after construction.
type entityMeta struct {
	entity   *dsl.Entity
	entityID string

	// SQL strings, built once at startup.
	sqlGet, sqlBatchGet, sqlQueryPrefix string
	sqlInsert, sqlUpdate, sqlDelete     string

	// Proto descriptors for dynamicpb message construction.
	fileDesc   protoreflect.FileDescriptor
	msgDesc    protoreflect.MessageDescriptor
	filterDesc protoreflect.MessageDescriptor

	// Request/response descriptors, keyed by RPC verb prefix.
	getRequestDesc, getResponseDesc           protoreflect.MessageDescriptor
	createRequestDesc, createResponseDesc     protoreflect.MessageDescriptor
	updateRequestDesc, updateResponseDesc     protoreflect.MessageDescriptor
	deleteRequestDesc, deleteResponseDesc     protoreflect.MessageDescriptor
	batchGetRequestDesc, batchGetResponseDesc protoreflect.MessageDescriptor
	queryRequestDesc, queryResponseDesc       protoreflect.MessageDescriptor

	// Service descriptor for gRPC registration.
	svcDesc protoreflect.ServiceDescriptor

	// Query machinery.
	filterSpec query.FilterSpec
	timeoutMS  int

	// Column metadata ordered to match the SELECT list.
	columns    []columnMeta
	insertCols []columnMeta
	updateCols []columnMeta
	pkCols     []columnMeta
}

// columnMeta is per-column metadata used by scan and bind helpers.
type columnMeta struct {
	field    *dsl.Field
	protoNum protoreflect.FieldNumber
	sqlName  string
	nullable bool
}

// buildEntityMeta constructs the full entityMeta for one entity.
func buildEntityMeta(e *dsl.Entity, ir *dsl.IR) *entityMeta {
	meta := &entityMeta{
		entity:   e,
		entityID: e.ID(),
	}

	// Timeout: default 2000ms if not overridden.
	meta.timeoutMS = e.QueryTimeoutMS
	if meta.timeoutMS == 0 {
		meta.timeoutMS = 2000
	}

	// Column metadata: all fields in declaration order.
	meta.columns = buildColumnMeta(e)

	// Insert columns: excludes serial/identity.
	for i := range meta.columns {
		cm := &meta.columns[i]
		if cm.field.Identity || cm.field.Serial {
			continue
		}
		meta.insertCols = append(meta.insertCols, *cm)
	}

	// Update columns: excludes PK, serial, and identity.
	for i := range meta.columns {
		cm := &meta.columns[i]
		if cm.field.Identity || cm.field.Serial || schema.IsPKColumn(e, cm.field.Name) {
			continue
		}
		meta.updateCols = append(meta.updateCols, *cm)
	}

	// PK columns.
	pkFields := schema.PKColumns(e)
	for _, pkf := range pkFields {
		for i := range meta.columns {
			if meta.columns[i].field.Name == pkf.Name {
				meta.pkCols = append(meta.pkCols, meta.columns[i])
				break
			}
		}
	}

	// FilterSpec for TranslateFilter.
	meta.filterSpec = buildFilterSpec(e)

	meta.sqlGet = buildGetSQL(e)
	meta.sqlBatchGet = buildBatchGetSQL(e)
	meta.sqlQueryPrefix = buildQueryPrefix(e)
	meta.sqlInsert = buildInsertSQL(e)
	meta.sqlUpdate = buildUpdateSQL(e)
	meta.sqlDelete = buildDeleteSQL(e)

	return meta
}

// resolveProtoDescriptors populates meta from the file descriptor.
func resolveProtoDescriptors(meta *entityMeta, fd protoreflect.FileDescriptor) {
	meta.fileDesc = fd
	name := meta.entity.Name

	meta.msgDesc = fd.Messages().ByName(protoreflect.Name(name))
	meta.filterDesc = fd.Messages().ByName(protoreflect.Name(name + "Filter"))

	meta.getRequestDesc = fd.Messages().ByName(protoreflect.Name("Get" + name + "Request"))
	meta.getResponseDesc = fd.Messages().ByName(protoreflect.Name("Get" + name + "Response"))
	meta.createRequestDesc = fd.Messages().ByName(protoreflect.Name("Create" + name + "Request"))
	meta.createResponseDesc = fd.Messages().ByName(protoreflect.Name("Create" + name + "Response"))
	meta.updateRequestDesc = fd.Messages().ByName(protoreflect.Name("Update" + name + "Request"))
	meta.updateResponseDesc = fd.Messages().ByName(protoreflect.Name("Update" + name + "Response"))
	meta.deleteRequestDesc = fd.Messages().ByName(protoreflect.Name("Delete" + name + "Request"))
	meta.deleteResponseDesc = fd.Messages().ByName(protoreflect.Name("Delete" + name + "Response"))
	meta.batchGetRequestDesc = fd.Messages().ByName(protoreflect.Name("BatchGet" + name + "Request"))
	meta.batchGetResponseDesc = fd.Messages().ByName(protoreflect.Name("BatchGet" + name + "Response"))
	meta.queryRequestDesc = fd.Messages().ByName(protoreflect.Name("Query" + name + "Request"))
	meta.queryResponseDesc = fd.Messages().ByName(protoreflect.Name("Query" + name + "Response"))

	meta.svcDesc = fd.Services().ByName(protoreflect.Name(name + "Service"))
}

func buildColumnMeta(e *dsl.Entity) []columnMeta {
	cols := make([]columnMeta, len(e.Fields))
	for i := range e.Fields {
		f := &e.Fields[i]
		cols[i] = columnMeta{
			field:    f,
			protoNum: protoreflect.FieldNumber(f.ProtoNumber),
			sqlName:  f.Name,
			nullable: schema.IsEffectivelyNullable(f),
		}
	}
	return cols
}

// buildFilterSpec mirrors the codegen's emitFilterSpec.
func buildFilterSpec(e *dsl.Entity) query.FilterSpec {
	fields := make(map[string]query.FieldSpec)
	for _, f := range e.Fields {
		kindStr, ok := schema.PredicateKindForField(f.Type)
		if !ok {
			continue
		}
		kind := predicateKindFromString(kindStr)
		fields[f.Name] = query.FieldSpec{
			Column: f.Name,
			Kind:   kind,
		}
	}
	return query.FilterSpec{
		EntityID:  e.ID(),
		TableName: schema.TableName(e),
		Fields:    fields,
	}
}

func predicateKindFromString(s string) query.PredicateKind {
	switch s {
	case "PredicateString":
		return query.PredicateString
	case "PredicateInt32":
		return query.PredicateInt32
	case "PredicateInt64":
		return query.PredicateInt64
	case "PredicateBool":
		return query.PredicateBool
	case "PredicateTimestamp":
		return query.PredicateTimestamp
	case "PredicateBytes":
		return query.PredicateBytes
	case "PredicateNumeric":
		return query.PredicateNumeric
	}
	return query.PredicateUnknown
}
