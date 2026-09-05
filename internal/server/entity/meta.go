// Package entity provides runtime schema dispatch for atlantis entity
// CRUD. Instead of tens of thousands of lines of generated Go, this
// package reads the DSL IR at startup and serves every entity RPC
// dynamically.
package entity

import (
	"github.com/rachitkumar205/atlantis/internal/codegen/query"
	"github.com/rachitkumar205/atlantis/internal/coltype"
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

	// inbound are the cross-entity invalidation rules that fire when this
	// entity is written; inboundCols are the columns they read, in the order
	// the write SQL returns them. Both empty for the vast majority of
	// entities, and every code path they drive is skipped when they are.
	inbound []inboundRule
	// inboundCols are the columns the rules read, and inboundColMeta is their
	// column metadata in the same order — needed so the values are scanned
	// into the same Go types the rest of the system uses when it builds a
	// cache id. See makeScanTargets.
	inboundCols    []string
	inboundColMeta []columnMeta
	// cacheable is false when the read cache cannot safely serve this entity.
	//
	// Two reasons, and they fail differently:
	//
	//   - A procedure writes it. Procedures cannot invalidate row bodies — see
	//     procedureWrittenEntities — so the cache would serve stale rows.
	//
	//   - It declares `partition by`. The cache sits ABOVE SQL: a hit never
	//     reaches the database, so row-level security never runs. The key is
	//     entity plus primary key with no tenant in it, so one tenant's cached
	//     row would be served to another asking for the same id — a
	//     cross-tenant read that the policy cannot prevent, because the policy
	//     is never consulted.
	//
	// Putting the tenant in the key is the obvious repair and does not work
	// yet: the asynchronous invalidation worker drains an outbox and has no
	// request context, so it cannot construct a tenant-scoped key. Its
	// invalidations would miss, and the entries would go stale permanently —
	// worse than not caching. Making the outbox carry the tenant is the way
	// back to a cache here.
	cacheable bool
	// partitioned is true when the entity declares `partition by`, so every
	// statement against it must run inside a transaction with the caller's
	// tenant bound.
	//
	// A resolved bool rather than a nil-check on entity: the first version of
	// the dispatcher guarded on `meta.entity == nil`, a condition
	// buildEntityMeta cannot produce, so the untaken branch silently disabled
	// enforcement for an input that never arrives. A guard that cannot fire is
	// indistinguishable from one that fires and finds nothing wrong.
	partitioned bool
	// sqlWriteBack reads a row back inside the transaction that wrote it.
	// Same projection as sqlGet, without the soft-delete filter. See
	// buildWriteBackSQL.
	sqlWriteBack string
	// sqlSelectInbound reads inboundCols for one row, FOR UPDATE. Used before
	// an UPDATE to capture the pre-update parent key, so reparenting a child
	// invalidates the parent it left as well as the one it joined.
	sqlSelectInbound string

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

	// orderCols resolves an <Entity>OrderField enum number to the column it
	// names. The enum carries each field's proto number, so the key is the
	// proto number and no second numbering exists to drift.
	//
	// A variant absent from this map is one the caller invented; handleQuery
	// rejects it rather than dropping it, because a dropped ORDER BY returns
	// rows in a different order than asked for and says nothing.
	orderCols map[protoreflect.EnumNumber]orderColumn

	// pkOrderCols are the primary key's columns, in declaration order. Any of
	// them the caller's ORDER BY does not already name is appended to it, so
	// the keyset always ends on a total order and every page advances.
	pkOrderCols []orderColumn
}

// orderColumn is one column a caller may sort on.
type orderColumn struct {
	quotedIdent string
	// nullable reads the DSL declaration, not schema.IsEffectivelyNullable.
	//
	// The two disagree on a column with a default, which that function reports
	// as nullable because it is describing the write path. Here the flag
	// selects the SHAPE of the keyset predicate, and the answer wanted is
	// whether Postgres can store a NULL in the column.
	nullable bool
	// protoNum locates the column on the entity message when the boundary
	// row's cursor coordinates are read back out.
	protoNum protoreflect.FieldNumber
}

// columnMeta is per-column metadata used by scan and bind helpers.
type columnMeta struct {
	field    *dsl.Field
	protoNum protoreflect.FieldNumber
	sqlName  string
	nullable bool
}

// buildEntityMeta constructs the full entityMeta for one entity.
func buildEntityMeta(e *dsl.Entity, ir *dsl.IR, inbound map[string][]inboundRule, procWritten map[string]bool) *entityMeta {
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

	meta.orderCols = buildOrderColumns(e)
	for _, pk := range meta.pkCols {
		meta.pkOrderCols = append(meta.pkOrderCols, orderColumn{
			quotedIdent: schema.QuoteIdent(pk.sqlName),
			// Never nullable, and not because the DSL says so — PRIMARY KEY
			// implies NOT NULL in Postgres whatever the declaration carries.
			nullable: false,
			protoNum: pk.protoNum,
		})
	}

	meta.sqlGet = buildGetSQL(e)
	meta.sqlWriteBack = buildWriteBackSQL(e)
	meta.sqlBatchGet = buildBatchGetSQL(e)
	meta.sqlQueryPrefix = buildQueryPrefix(e)
	meta.cacheable = !procWritten[e.ID()] && e.PartitionField == ""
	meta.partitioned = e.PartitionField != ""
	meta.inbound = inbound[e.ID()]
	meta.inboundCols = inboundColumns(meta.inbound)
	for _, name := range meta.inboundCols {
		for i := range meta.columns {
			if meta.columns[i].sqlName == name {
				meta.inboundColMeta = append(meta.inboundColMeta, meta.columns[i])
				break
			}
		}
	}
	// A rule naming a column this entity does not have cannot be scanned, and
	// carrying half a rule set would scan values into the wrong slots. Drop the
	// whole set rather than emit invalidations for the wrong parents.
	if len(meta.inboundColMeta) != len(meta.inboundCols) {
		meta.inbound = nil
		meta.inboundCols = nil
		meta.inboundColMeta = nil
	}

	meta.sqlInsert = buildInsertSQL(e, meta.inboundCols)
	meta.sqlUpdate = buildUpdateSQL(e, meta.inboundCols)
	meta.sqlDelete = buildDeleteSQL(e, meta.inboundCols)
	meta.sqlSelectInbound = buildSelectInboundSQL(e, meta.inboundCols)

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

// buildOrderColumns indexes the orderable columns by the <Entity>OrderField
// enum number that names them, which is the field's proto number.
//
// Keyed on the number rather than the variant name so a renamed column keeps
// working for a client compiled against the old proto — the same property the
// entity message itself has.
func buildOrderColumns(e *dsl.Entity) map[protoreflect.EnumNumber]orderColumn {
	out := make(map[protoreflect.EnumNumber]orderColumn, len(e.Fields))
	for i := range e.Fields {
		f := &e.Fields[i]
		if !coltype.Orderable(f.Type) {
			continue
		}
		out[protoreflect.EnumNumber(f.ProtoNumber)] = orderColumn{
			quotedIdent: schema.QuoteIdent(f.Name),
			nullable:    !f.NotNull,
			protoNum:    protoreflect.FieldNumber(f.ProtoNumber),
		}
	}
	return out
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
	case "PredicateFloat":
		return query.PredicateFloat
	case "PredicateDouble":
		return query.PredicateDouble
	}
	return query.PredicateUnknown
}
