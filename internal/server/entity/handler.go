package entity

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/rachitkumar205/atlantis/internal/codegen/query"
	"github.com/rachitkumar205/atlantis/internal/runtime"
	"github.com/rachitkumar205/atlantis/internal/schema"
)

// dispatch routes an RPC to the correct handler based on the op string.
func (s *Server) dispatch(ctx context.Context, meta *entityMeta, op string, dec func(any) error) (any, error) {
	switch op {
	case "Get":
		return s.handleGet(ctx, meta, dec)
	case "Create":
		return s.handleCreate(ctx, meta, dec)
	case "Update":
		return s.handleUpdate(ctx, meta, dec)
	case "Delete":
		return s.handleDelete(ctx, meta, dec)
	case "BatchGet":
		return s.handleBatchGet(ctx, meta, dec)
	case "Query":
		return s.handleQuery(ctx, meta, dec)
	default:
		return nil, status.Errorf(codes.Unimplemented, "unknown operation %s%s", op, meta.entity.Name)
	}
}

// makeHandler returns a grpc.MethodDesc.Handler for one RPC method.
// It captures the entity ID (immutable string) rather than a pointer
// to entityMeta, and loads the current metadata from the snapshot at
// each request. This allows the snapshot to be swapped for hot-reload.
func makeHandler(s *Server, entityID string, op string, ns string, name string) func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	fullMethod := fmt.Sprintf("/atlantis.%s.v1.%sService/%s%s", ns, name, op, name)

	return func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
		// The lookup runs inside the intercepted handler, so an entity the
		// current snapshot no longer holds is refused after the chain ran.
		handler := func(ctx context.Context, _ any) (any, error) {
			meta, ok := s.snapshot.Load().entities[entityID]
			if !ok {
				return nil, status.Errorf(codes.NotFound, "entity %s not found in current schema", entityID)
			}
			return s.dispatch(ctx, meta, op, dec)
		}
		if interceptor == nil {
			return handler(ctx, nil)
		}
		info := &grpc.UnaryServerInfo{
			Server:     srv,
			FullMethod: fullMethod,
		}
		return interceptor(ctx, nil, info, handler)
	}
}

// handleGet reads one row by primary key, through the read cache when the
// entity is cacheable and a reader is configured.
func (s *Server) handleGet(ctx context.Context, meta *entityMeta, dec func(any) error) (any, error) {
	ctx, cancel := runtime.Deadline(ctx, meta.timeoutMS)
	defer cancel()

	req := dynamicpb.NewMessage(meta.getRequestDesc)
	if err := dec(req); err != nil {
		return nil, err
	}

	// Extract PK values from the request.
	pkArgs := make([]any, 0, len(meta.pkCols))
	for i, cm := range meta.pkCols {
		fd := meta.getRequestDesc.Fields().ByNumber(protoreflect.FieldNumber(i + 1))
		if fd == nil {
			return nil, fmt.Errorf("Get%s: missing PK field %s in request", meta.entity.Name, cm.sqlName)
		}
		pkArgs = append(pkArgs, goValueFromProto(req, fd, cm))
	}

	// Read through the cache when one is configured. The Reader owns tier-0
	// LRU, versioned-key indirection over memcached, singleflight per miss and
	// XFetch early refresh; all this path supplies is a loader and the codec.
	//
	// Nil reader means caching is off, which is what the sandbox and most tests
	// want, and the loader below is then the whole of the read. So does
	// !meta.cacheable: a procedure writes this entity and cannot invalidate the
	// row bodies it changes, so caching it would serve stale rows.
	loadRow := func(ctx context.Context) (*dynamicpb.Message, error) {
		var e *dynamicpb.Message
		// The scan runs INSIDE the scope. A runtime.Row is only valid while its
		// transaction is open, so reading it after the scope closed would be a
		// use-after-commit that compiles cleanly and fails at run time.
		err := s.scopedRead(ctx, meta, func(q querier) error {
			var serr error
			e, serr = scanRow(meta, q.QueryRow(ctx, meta.sqlGet, pkArgs...))
			return serr
		})
		if err != nil {
			if runtime.IsNoRows(err) {
				return nil, runtime.ErrNotFound
			}
			return nil, err
		}
		return e, nil
	}

	var entity *dynamicpb.Message
	if s.reader == nil || !meta.cacheable {
		var err error
		if entity, err = loadRow(ctx); err != nil {
			return nil, err
		}
	} else {
		cacheID := runtime.CompositeID(pkArgs...)
		body, err := s.reader.Get(ctx, meta.entityID, cacheID, func(ctx context.Context) ([]byte, error) {
			e, lerr := loadRow(ctx)
			if lerr != nil {
				return nil, lerr
			}
			return proto.Marshal(e)
		})
		if err != nil {
			// ErrNotFound travels up from the loader unchanged. Caching a
			// negative result would need a tombstone the invalidation path
			// knows how to clear, and it does not, so a missing row simply is
			// not cached.
			return nil, err
		}
		entity = dynamicpb.NewMessage(meta.msgDesc)
		if err := proto.Unmarshal(body, entity); err != nil {
			return nil, fmt.Errorf("Get%s: decode cached body: %w", meta.entity.Name, err)
		}
	}

	resp := dynamicpb.NewMessage(meta.getResponseDesc)
	entityFD := meta.getResponseDesc.Fields().ByName("entity")
	if entityFD != nil {
		resp.Set(entityFD, protoreflect.ValueOfMessage(entity))
	}
	return resp, nil
}

func (s *Server) handleCreate(ctx context.Context, meta *entityMeta, dec func(any) error) (any, error) {
	ctx, cancel := runtime.Deadline(ctx, meta.timeoutMS)
	defer cancel()

	req := dynamicpb.NewMessage(meta.createRequestDesc)
	if err := dec(req); err != nil {
		return nil, err
	}

	// Extract the entity sub-message from the "entity" field.
	entityFD := meta.createRequestDesc.Fields().ByName("entity")
	if entityFD == nil {
		return nil, fmt.Errorf("Create%s: request missing entity field", meta.entity.Name)
	}
	if !req.Has(entityFD) {
		return nil, fmt.Errorf("Create%s: entity is required", meta.entity.Name)
	}
	entityMsg, ok := req.Get(entityFD).Message().Interface().(*dynamicpb.Message)
	if !ok {
		return nil, fmt.Errorf("Create%s: entity is not a dynamic message", meta.entity.Name)
	}

	args := bindForInsert(meta, entityMsg)

	tx, err := s.pool.BeginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// Bind before any statement runs in this transaction: the policy applies
	// from the first statement, so anything issued ahead of the bind is
	// unscoped. A partitioned entity with no tenant in context is refused here
	// rather than reaching the database.
	if err := s.bindWrite(ctx, meta, tx); err != nil {
		return nil, err
	}

	// INSERT RETURNING pk, then any cross-entity invalidation columns. They
	// ride the same RETURNING clause, so learning the parent key costs no
	// extra round trip.
	pkScanTargets := makePKScanTargets(meta)
	inboundTargets := makeScanTargets(meta.inboundColMeta)
	// Cloned rather than appended to: append on a full slice would reallocate
	// here and not there, which works by luck rather than by construction.
	scanTargets := append(slices.Clone(pkScanTargets), inboundTargets...)
	row := tx.QueryRow(ctx, meta.sqlInsert, args...)
	if err := row.Scan(scanTargets...); err != nil {
		return nil, err
	}
	inboundVals := readScanTargets(meta.inboundColMeta, inboundTargets)

	// Build cache ID from returned PK.
	pkValues := readPKScanTargets(meta, pkScanTargets)
	cacheID := runtime.CompositeID(pkValues...)

	// Outbox invalidation.
	cur, _ := s.cache.CurrentVersion(ctx, meta.entityID, cacheID)
	if err := s.outbox.Enqueue(ctx, tx, meta.entityID, cacheID, cur+1); err != nil {
		return nil, err
	}
	if err := s.outbox.EnqueueGenerationBump(ctx, tx, meta.entityID); err != nil {
		return nil, err
	}
	if err := s.enqueueParents(ctx, tx, meta, inboundVals); err != nil {
		return nil, err
	}
	// Read the row back BEFORE committing, inside this transaction.
	//
	// It used to be read afterwards on the bare pool. For a partitioned entity
	// that is a different, unbound transaction: current_partition() is NULL
	// there, the policy matches nothing, and the refetch returns no rows — so
	// Create reported failure on a write that had already committed, and the
	// caller's retry hit a primary-key conflict. Reading inside is also simply
	// more correct: the row is returned from the same snapshot that wrote it.
	entity, err := scanRow(meta, tx.QueryRow(ctx, meta.sqlWriteBack, pkValues...))
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	resp := dynamicpb.NewMessage(meta.createResponseDesc)
	respEntityFD := meta.createResponseDesc.Fields().ByName("entity")
	if respEntityFD != nil {
		resp.Set(respEntityFD, protoreflect.ValueOfMessage(entity))
	}
	return resp, nil
}

func (s *Server) handleUpdate(ctx context.Context, meta *entityMeta, dec func(any) error) (any, error) {
	ctx, cancel := runtime.Deadline(ctx, meta.timeoutMS)
	defer cancel()

	if meta.sqlUpdate == "" {
		return nil, status.Errorf(codes.Unimplemented, "Update%s: entity has no updatable columns", meta.entity.Name)
	}

	req := dynamicpb.NewMessage(meta.updateRequestDesc)
	if err := dec(req); err != nil {
		return nil, err
	}

	entityFD := meta.updateRequestDesc.Fields().ByName("entity")
	if entityFD == nil {
		return nil, fmt.Errorf("Update%s: request missing entity field", meta.entity.Name)
	}
	if !req.Has(entityFD) {
		return nil, fmt.Errorf("Update%s: entity is required", meta.entity.Name)
	}
	entityMsg, ok := req.Get(entityFD).Message().Interface().(*dynamicpb.Message)
	if !ok {
		return nil, fmt.Errorf("Update%s: entity is not a dynamic message", meta.entity.Name)
	}

	mask := updateMaskPaths(req)
	if err := runtime.CheckFieldMask(mask, meta.updateColNames); err != nil {
		return nil, err
	}
	args := bindForUpdate(meta, entityMsg, mask)
	pkValues := extractPKValues(meta, entityMsg)

	tx, err := s.pool.BeginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// Bind before any statement runs in this transaction: the policy applies
	// from the first statement, so anything issued ahead of the bind is
	// unscoped. A partitioned entity with no tenant in context is refused here
	// rather than reaching the database.
	if err := s.bindWrite(ctx, meta, tx); err != nil {
		return nil, err
	}

	// The parent key as it stands BEFORE the update, read under FOR UPDATE.
	// Reparenting a child — moving a CartItem from cart 1 to cart 2 — has to
	// invalidate the cart it left as well as the one it joined, and the
	// UPDATE's RETURNING clause reports only the new value. No-op for entities
	// without inbound rules, which is nearly all of them.
	oldInbound, err := s.readInboundValues(ctx, tx, meta, pkValues)
	if err != nil {
		return nil, err
	}

	var inboundVals []any
	if len(meta.inboundCols) == 0 {
		tag, err := tx.Exec(ctx, meta.sqlUpdate, args...)
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() == 0 {
			return nil, runtime.ErrNotFound
		}
	} else {
		ptrs := makeScanTargets(meta.inboundColMeta)
		// RETURNING makes this a query, so "no rows" replaces RowsAffected()==0
		// as the not-found signal.
		if err := tx.QueryRow(ctx, meta.sqlUpdate, args...).Scan(ptrs...); err != nil {
			if runtime.IsNoRows(err) {
				return nil, runtime.ErrNotFound
			}
			return nil, err
		}
		inboundVals = readScanTargets(meta.inboundColMeta, ptrs)
	}

	cacheID := runtime.CompositeID(pkValues...)
	cur, _ := s.cache.CurrentVersion(ctx, meta.entityID, cacheID)
	if err := s.outbox.Enqueue(ctx, tx, meta.entityID, cacheID, cur+1); err != nil {
		return nil, err
	}
	if err := s.outbox.EnqueueGenerationBump(ctx, tx, meta.entityID); err != nil {
		return nil, err
	}
	if err := s.enqueueParents(ctx, tx, meta, oldInbound, inboundVals); err != nil {
		return nil, err
	}
	// Read back inside the transaction, for the same reason as handleCreate:
	// on a partitioned entity a post-commit read on the pool is unbound and
	// returns nothing, so Update reported failure on a successful write.
	entity, err := scanRow(meta, tx.QueryRow(ctx, meta.sqlWriteBack, pkValues...))
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	resp := dynamicpb.NewMessage(meta.updateResponseDesc)
	respEntityFD := meta.updateResponseDesc.Fields().ByName("entity")
	if respEntityFD != nil {
		resp.Set(respEntityFD, protoreflect.ValueOfMessage(entity))
	}
	return resp, nil
}

func (s *Server) handleDelete(ctx context.Context, meta *entityMeta, dec func(any) error) (any, error) {
	ctx, cancel := runtime.Deadline(ctx, meta.timeoutMS)
	defer cancel()

	req := dynamicpb.NewMessage(meta.deleteRequestDesc)
	if err := dec(req); err != nil {
		return nil, err
	}

	pkArgs := make([]any, 0, len(meta.pkCols))
	for i, cm := range meta.pkCols {
		fd := meta.deleteRequestDesc.Fields().ByNumber(protoreflect.FieldNumber(i + 1))
		if fd == nil {
			return nil, fmt.Errorf("Delete%s: missing PK field %s", meta.entity.Name, cm.sqlName)
		}
		pkArgs = append(pkArgs, goValueFromProto(req, fd, cm))
	}

	tx, err := s.pool.BeginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// Bind before any statement runs in this transaction: the policy applies
	// from the first statement, so anything issued ahead of the bind is
	// unscoped. A partitioned entity with no tenant in context is refused here
	// rather than reaching the database.
	if err := s.bindWrite(ctx, meta, tx); err != nil {
		return nil, err
	}

	var delInbound []any
	if len(meta.inboundCols) == 0 {
		tag, err := tx.Exec(ctx, meta.sqlDelete, pkArgs...)
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() == 0 {
			return nil, runtime.ErrNotFound
		}
	} else {
		// The row is going away, so its parent key has to be captured on the
		// way out; there is nothing left to read afterwards.
		ptrs := makeScanTargets(meta.inboundColMeta)
		if err := tx.QueryRow(ctx, meta.sqlDelete, pkArgs...).Scan(ptrs...); err != nil {
			if runtime.IsNoRows(err) {
				return nil, runtime.ErrNotFound
			}
			return nil, err
		}
		delInbound = readScanTargets(meta.inboundColMeta, ptrs)
	}

	cacheID := runtime.CompositeID(pkArgs...)
	cur, _ := s.cache.CurrentVersion(ctx, meta.entityID, cacheID)
	if err := s.outbox.Enqueue(ctx, tx, meta.entityID, cacheID, cur+1); err != nil {
		return nil, err
	}
	if err := s.outbox.EnqueueGenerationBump(ctx, tx, meta.entityID); err != nil {
		return nil, err
	}
	if err := s.enqueueParents(ctx, tx, meta, delInbound); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	resp := dynamicpb.NewMessage(meta.deleteResponseDesc)
	return resp, nil
}

// handleBatchGet uses ANY($1) for single-PK entities; composite PKs
// fall back to individual gets.
func (s *Server) handleBatchGet(ctx context.Context, meta *entityMeta, dec func(any) error) (any, error) {
	ctx, cancel := runtime.Deadline(ctx, meta.timeoutMS)
	defer cancel()

	req := dynamicpb.NewMessage(meta.batchGetRequestDesc)
	if err := dec(req); err != nil {
		return nil, err
	}

	resp := dynamicpb.NewMessage(meta.batchGetResponseDesc)
	entitiesFD := meta.batchGetResponseDesc.Fields().ByName("entities")
	if entitiesFD == nil {
		return resp, nil
	}

	composite := len(meta.pkCols) > 1

	// One scope around both branches, not one per id. The composite-PK branch
	// below reads one row per id and has no cap on how many, so a partitioned
	// BatchGet would otherwise open a transaction per id — and they would not
	// share a snapshot, so the same call could see a row through one and not
	// another. (The 200 cap applies only to the single-PK branch, which issues
	// one query.)
	if err := s.scopedRead(ctx, meta, func(q querier) error {
		return s.batchGetInto(ctx, q, meta, req, resp, entitiesFD, composite)
	}); err != nil {
		return nil, err
	}
	return resp, nil
}

// batchGetInto fills resp's entity list. Split out so the whole read runs in
// one scope; see handleBatchGet.
func (s *Server) batchGetInto(
	ctx context.Context,
	q querier,
	meta *entityMeta,
	req, resp *dynamicpb.Message,
	entitiesFD protoreflect.FieldDescriptor,
	composite bool,
) error {
	if composite {
		// Composite PK: individual gets.
		idsFD := meta.batchGetRequestDesc.Fields().ByName("ids")
		if idsFD == nil {
			return nil
		}
		list := req.Get(idsFD).List()
		entities := resp.Mutable(entitiesFD).List()
		for i := 0; i < list.Len(); i++ {
			pkMsg := list.Get(i).Message()
			pkArgs := make([]any, len(meta.pkCols))
			for j := range meta.pkCols {
				pkFD := pkMsg.Descriptor().Fields().ByNumber(protoreflect.FieldNumber(j + 1))
				if pkFD != nil {
					pkArgs[j] = goValueFromProtoReflect(pkMsg, pkFD, meta.pkCols[j])
				}
			}
			row := q.QueryRow(ctx, meta.sqlGet, pkArgs...)
			entity, err := scanRow(meta, row)
			if err != nil {
				if runtime.IsNoRows(err) {
					continue
				}
				return err
			}
			entities.Append(protoreflect.ValueOfMessage(entity))
		}
	} else {
		// Single PK: use ANY($1).
		pkField := meta.batchGetRequestDesc.Fields().Get(0) // first field is the repeated PK
		if pkField == nil {
			return nil
		}
		list := req.Get(pkField).List()
		if list.Len() == 0 {
			return nil
		}
		if list.Len() > 200 {
			return status.Errorf(codes.InvalidArgument,
				"BatchGet%s: at most 200 ids per call (got %d)", meta.entity.Name, list.Len())
		}

		// Build the array arg.
		pkSlice, err := buildPKArray(meta.pkCols[0], list)
		if err != nil {
			return err
		}

		rows, err := q.Query(ctx, meta.sqlBatchGet, pkSlice)
		if err != nil {
			return err
		}
		defer rows.Close()

		entities := resp.Mutable(entitiesFD).List()
		for rows.Next() {
			entity, err := scanRow(meta, rows)
			if err != nil {
				return err
			}
			entities.Append(protoreflect.ValueOfMessage(entity))
		}
		if err := rows.Err(); err != nil {
			return err
		}
	}

	return nil
}

// handleQuery applies TranslateFilter + keyset pagination.
func (s *Server) handleQuery(ctx context.Context, meta *entityMeta, dec func(any) error) (any, error) {
	ctx, cancel := runtime.Deadline(ctx, meta.timeoutMS)
	defer cancel()

	req := dynamicpb.NewMessage(meta.queryRequestDesc)
	if err := dec(req); err != nil {
		return nil, err
	}

	// Extract limit.
	limitFD := meta.queryRequestDesc.Fields().ByName("limit")
	limit := int32(100)
	if limitFD != nil && req.Has(limitFD) {
		limit = int32(req.Get(limitFD).Int())
	}
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}

	// Extract page_token.
	pageTokenFD := meta.queryRequestDesc.Fields().ByName("page_token")
	pageToken := ""
	if pageTokenFD != nil && req.Has(pageTokenFD) {
		pageToken = req.Get(pageTokenFD).String()
	}

	// Extract filter.
	filterFD := meta.queryRequestDesc.Fields().ByName("filter")
	var filterMsg protoreflect.Message
	if filterFD != nil && req.Has(filterFD) {
		filterMsg = req.Get(filterFD).Message()
	}

	// A projection or an eager-load the dispatcher cannot perform is refused
	// here. Both fields are declared on the request, so a caller that sets one
	// gets this error; leaving them off the descriptor instead would drop them
	// as unknown fields and return a full row set that looks like an answer.
	if err := rejectUnsupportedQueryOptions(meta, req); err != nil {
		return nil, err
	}

	keysetCols, orderCols, err := buildKeysetCols(meta, req)
	if err != nil {
		return nil, err
	}

	// Decode cursor.
	cursorVals, err := runtime.DecodePageToken(pageToken, meta.entityID)
	if err != nil {
		return nil, err
	}
	if err := checkCursorArity(cursorVals, keysetCols); err != nil {
		return nil, err
	}

	// Translate filter.
	extras := make([]string, 0, 2)

	// Soft delete filter.
	if meta.entity.SoftDeleteField != "" {
		extras = append(extras, schema.QuoteIdent(meta.entity.SoftDeleteField)+" IS NULL")
	}

	where, args, _, err := query.TranslateFilter(meta.filterSpec, filterMsg, 1, extras...)
	if err != nil {
		return nil, err
	}

	// Keyset predicate.
	if len(cursorVals) > 0 {
		cursorSQL, cursorArgs, kerr := runtime.KeysetPredicate(keysetCols, cursorVals, len(args)+1)
		if kerr != nil {
			return nil, kerr
		}
		if where == "" {
			where = cursorSQL
		} else {
			where = where + " AND " + cursorSQL
		}
		args = append(args, cursorArgs...)
	}

	// Assemble SQL.
	var b strings.Builder
	b.WriteString(meta.sqlQueryPrefix)
	if where != "" {
		b.WriteString(" WHERE ")
		b.WriteString(where)
	}
	b.WriteString(runtime.OrderByClauseFromKeyset(keysetCols))
	args = append(args, limit+1)
	fmt.Fprintf(&b, " LIMIT $%d", len(args))
	sqlText := b.String()

	resp := dynamicpb.NewMessage(meta.queryResponseDesc)
	entitiesFD := meta.queryResponseDesc.Fields().ByName("entities")
	if entitiesFD == nil {
		return resp, nil
	}
	entities := resp.Mutable(entitiesFD).List()

	// Rows are scanned inside the scope. runtime.Rows is only valid while its
	// transaction is open, and the pagination below reads `entities`, which is
	// already materialised by then.
	if err := s.scopedRead(ctx, meta, func(q querier) error {
		rows, err := q.Query(ctx, sqlText, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			entity, err := scanRow(meta, rows)
			if err != nil {
				return err
			}
			entities.Append(protoreflect.ValueOfMessage(entity))
		}
		return rows.Err()
	}); err != nil {
		return nil, err
	}

	// More rows than the limit means another page: trim and set
	// next_page_token.
	if int32(entities.Len()) > limit {
		// Extract cursor from the boundary row (the limit-th entity, 0-indexed).
		boundaryEntity := entities.Get(int(limit) - 1).Message().Interface().(*dynamicpb.Message)

		// Trim to limit.
		entities.Truncate(int(limit))

		nextToken, err := nextPageToken(meta, boundaryEntity, orderCols)
		if err != nil {
			return nil, err
		}
		nextTokenFD := meta.queryResponseDesc.Fields().ByName("next_page_token")
		if nextTokenFD != nil {
			resp.Set(nextTokenFD, protoreflect.ValueOfString(nextToken))
		}
	}

	return resp, nil
}

// buildKeysetCols renders the request's ORDER BY as a keyset column list,
// returning it alongside the orderColumn for each entry so the boundary row's
// cursor can be read back without a second lookup.
//
// Every primary-key column the request does not already name is appended.
// Ordering by a non-unique column alone gives no total order, so two rows
// sharing the boundary value straddle the page break: without the tiebreaker
// the cursor cannot say which of them was already returned, and the next page
// either repeats them or skips them.
//
// A variant the entity does not declare is rejected. Skipping it would serve
// a different ORDER BY than the one asked for and report success.
func buildKeysetCols(meta *entityMeta, req *dynamicpb.Message) ([]runtime.KeysetColumn, []orderColumn, error) {
	orderFD := meta.queryRequestDesc.Fields().ByName("order")

	var reqOrder protoreflect.List
	if orderFD != nil && req.Has(orderFD) {
		reqOrder = req.Get(orderFD).List()
	}
	n := 0
	if reqOrder != nil {
		n = reqOrder.Len()
	}

	cols := make([]runtime.KeysetColumn, 0, n+len(meta.pkOrderCols))
	ordered := make([]orderColumn, 0, n+len(meta.pkOrderCols))
	named := make(map[string]bool, n)

	for i := range n {
		ob := reqOrder.Get(i).Message()
		fieldFD := ob.Descriptor().Fields().ByName("field")
		descFD := ob.Descriptor().Fields().ByName("desc")
		if fieldFD == nil {
			continue
		}
		num := ob.Get(fieldFD).Enum()
		// UNSPECIFIED is the proto3 zero, so it is also what an OrderBy with
		// no field set reads as. Treated as "no column", not as an error: the
		// entry names nothing to sort by and dropping it changes no ordering.
		if num == 0 {
			continue
		}
		oc, ok := meta.orderCols[num]
		if !ok {
			return nil, nil, status.Errorf(codes.InvalidArgument,
				"entity %s: order field %d is not an orderable column", meta.entityID, num)
		}
		desc := descFD != nil && ob.Get(descFD).Bool()
		cols = append(cols, runtime.KeysetColumn{
			QuotedIdent: oc.quotedIdent,
			Desc:        desc,
			Nullable:    oc.nullable,
		})
		ordered = append(ordered, oc)
		named[oc.quotedIdent] = true
	}

	for _, pk := range meta.pkOrderCols {
		if named[pk.quotedIdent] {
			continue
		}
		cols = append(cols, runtime.KeysetColumn{
			QuotedIdent: pk.quotedIdent,
			Desc:        false,
			Nullable:    false,
		})
		ordered = append(ordered, pk)
	}
	return cols, ordered, nil
}

// checkCursorArity refuses a page token holding a different number of
// coordinates than the query has ordering columns.
//
// A cursor is positional: KeysetPredicate pairs value i with column i. The
// reachable cause is a caller changing `order` while paging, which invalidates
// the token it is still echoing.
//
// KeysetPredicate rejects the same mismatch further down, so this is the
// status code and the message rather than the detection: that error reads
// "runtime: KeysetPredicate: 2 cols vs 3 cursor values", which names an
// internal function and reaches the caller as Unknown.
//
// It does not catch a token replayed under a DIFFERENT ordering of the same
// length. Those coordinates are compared against columns they did not come
// from — an error from Postgres where the types disagree, and a page starting
// at the wrong row where they happen to match.
func checkCursorArity(cursor []any, cols []runtime.KeysetColumn) error {
	if len(cursor) == 0 || len(cursor) == len(cols) {
		return nil
	}
	return status.Errorf(codes.InvalidArgument,
		"page_token carries %d ordering values but the query has %d ordering columns; "+
			"a page_token is only valid with the `order` it was issued under",
		len(cursor), len(cols))
}

// rejectUnsupportedQueryOptions refuses a request naming a query option the
// dispatcher does not implement.
//
// `fields` would project a subset of columns and `includes` would eager-load a
// related entity. Both are declared on QueryXRequest and neither is served.
func rejectUnsupportedQueryOptions(meta *entityMeta, req *dynamicpb.Message) error {
	fieldsFD := meta.queryRequestDesc.Fields().ByName("fields")
	if fieldsFD != nil && req.Has(fieldsFD) {
		return status.Errorf(codes.Unimplemented,
			"entity %s: query field mask is not supported; omit `fields` to read every column", meta.entityID)
	}
	includesFD := meta.queryRequestDesc.Fields().ByName("includes")
	if includesFD != nil && req.Get(includesFD).List().Len() > 0 {
		return status.Errorf(codes.Unimplemented,
			"entity %s: query includes are not supported; read the related entity with its own query", meta.entityID)
	}
	return nil
}

// nextPageToken renders the cursor for the boundary row of a page.
//
// Extracted from handleQuery so the error path has somewhere to be tested from.
// Both halves used to be inline, and the encode's error was assigned to `_`:
// the token came back as the empty string, which on the wire is
// indistinguishable from "that was the last page". A caller that trusted it
// stopped early believing it had read everything, and nothing — not a log line,
// not a metric, not a status code — said otherwise. Returning the error makes
// one request fail loudly instead of every request after it lying quietly.
func nextPageToken(meta *entityMeta, boundary *dynamicpb.Message, cols []orderColumn) (string, error) {
	vals, err := extractCursorValues(meta, boundary, cols)
	if err != nil {
		return "", err
	}
	return runtime.EncodePageToken(meta.entityID, vals)
}

// extractCursorValues reads the boundary row's coordinate for each keyset
// column, in the order the ORDER BY names them.
//
// A column with no matching entry in meta.columns is an error rather than a
// skip. Silently emitting a shorter slice produces a token whose arity does not
// match the column list, and nothing rejects that until the NEXT request
// decodes it — at which point the failure names KeysetPredicate and points
// nowhere near the entity whose metadata is inconsistent.
func extractCursorValues(meta *entityMeta, entity *dynamicpb.Message, cols []orderColumn) ([]any, error) {
	out := make([]any, 0, len(cols))
	for _, oc := range cols {
		found := false
		for _, cm := range meta.columns {
			if cm.protoNum == oc.protoNum {
				fd := meta.msgDesc.Fields().ByNumber(cm.protoNum)
				out = append(out, protoValueForCursor(entity, fd, cm))
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("entity %s: keyset column %s has no column metadata", meta.entityID, oc.quotedIdent)
		}
	}
	return out, nil
}

// goValueFromProto extracts a Go value for SQL arguments.
func goValueFromProto(msg *dynamicpb.Message, fd protoreflect.FieldDescriptor, cm columnMeta) any {
	return goValueFromProtoReflect(msg, fd, cm)
}

func goValueFromProtoReflect(msg protoreflect.Message, fd protoreflect.FieldDescriptor, cm columnMeta) any {
	t := cm.field.Type
	switch t.Name {
	case "text", "varchar", "citext", "uuid", "numeric", "interval":
		return msg.Get(fd).String()
	case "bigint":
		return msg.Get(fd).Int()
	case "int", "smallint":
		return int32(msg.Get(fd).Int())
	case "boolean":
		return msg.Get(fd).Bool()
	case "real":
		// Explicit, even though the Interface() fallback below already yields
		// float32 for a TYPE_FLOAT field. The cache id is built from whatever
		// this returns, and readScanTargets must return the identical Go type
		// for the same row — leaving either side to a fallback is how those
		// two drifted apart before.
		return float32(msg.Get(fd).Float())
	case "double":
		return msg.Get(fd).Float()
	case "timestamptz", "date":
		return timestampToTime(msg, fd)
	case "bytea", "jsonb":
		return msg.Get(fd).Bytes()
	}
	return msg.Get(fd).Interface()
}

// buildPKArray builds a typed Go slice for the ANY($1) SQL pattern.
//
// The fourth primary-key type table in this file, and it carries the same list
// as the other three. A short list produces two failures on primary-key types
// the DSL accepts and lowers without complaint:
//
//   - A bytea key goes through fmt.Sprintf("%v", []byte{…}) and reaches
//     Postgres as the Go rendering "[222 173 190 239]". BatchGet returns zero
//     rows with a nil error: the caller asked for a key that exists and is
//     told it does not.
//   - A jsonb key renders the same way and fails with "invalid input syntax
//     for type json", which is at least loud.
//
// boolean and real survive a short list only because Postgres coerces an
// unknown-typed literal like "true" or "1e+06", which nothing here arranges.
func buildPKArray(pk columnMeta, list protoreflect.List) (any, error) {
	switch pk.field.Type.Name {
	case "text", "varchar", "citext", "uuid", "numeric":
		out := make([]string, list.Len())
		for i := 0; i < list.Len(); i++ {
			out[i] = list.Get(i).String()
		}
		return out, nil
	case "bigint":
		out := make([]int64, list.Len())
		for i := 0; i < list.Len(); i++ {
			out[i] = list.Get(i).Int()
		}
		return out, nil
	case "int", "smallint":
		out := make([]int32, list.Len())
		for i := 0; i < list.Len(); i++ {
			out[i] = int32(list.Get(i).Int())
		}
		return out, nil
	case "real":
		out := make([]float32, list.Len())
		for i := 0; i < list.Len(); i++ {
			out[i] = float32(list.Get(i).Float())
		}
		return out, nil
	case "double":
		out := make([]float64, list.Len())
		for i := 0; i < list.Len(); i++ {
			out[i] = list.Get(i).Float()
		}
		return out, nil
	case "boolean":
		out := make([]bool, list.Len())
		for i := 0; i < list.Len(); i++ {
			out[i] = list.Get(i).Bool()
		}
		return out, nil
	case "bytea", "jsonb":
		out := make([][]byte, list.Len())
		for i := 0; i < list.Len(); i++ {
			out[i] = list.Get(i).Bytes()
		}
		return out, nil
	case "timestamptz", "date":
		out := make([]time.Time, list.Len())
		for i := 0; i < list.Len(); i++ {
			sub := list.Get(i).Message()
			secFD := sub.Descriptor().Fields().ByName("seconds")
			nanoFD := sub.Descriptor().Fields().ByName("nanos")
			var sec, nanos int64
			if secFD != nil {
				sec = sub.Get(secFD).Int()
			}
			if nanoFD != nil {
				nanos = sub.Get(nanoFD).Int()
			}
			out[i] = time.Unix(sec, nanos).UTC()
		}
		return out, nil
	}
	// Refused rather than stringified. The previous fallback built a []string
	// via fmt.Sprintf("%v", …) and handed it to Postgres uncast, which is how
	// a bytea key turned into a silent empty result. An unsupported key type
	// is a schema atlantis cannot serve, and saying so beats answering wrongly.
	return nil, status.Errorf(codes.Unimplemented,
		"BatchGet: primary key type %q is not supported for batch lookup",
		pk.field.Type.Name)
}

// makePKScanTargets allocates scan targets for INSERT ... RETURNING.
func makePKScanTargets(meta *entityMeta) []any {
	return makeScanTargets(meta.pkCols)
}

// makeScanTargets allocates typed scan destinations for a set of columns.
//
// Typed, not []*any. Scanning into *any lets pgx pick the Go representation,
// which does not match what the rest of the system uses: a uuid arrives as
// [16]uint8, a numeric as pgtype.Numeric, a timestamptz as time.Time, a jsonb
// as map[string]any. Cache ids elsewhere are built from goValueFromProto, so
// the same row produces two different ids — "49:[17 17 17 …]" from one path and
// "36:1111-2222-…" from the other. The mismatch is silent: an outbox row is
// written, the worker bumps a pointer key nobody reads, and the cached row
// stays stale until TTL.
//
// The arms below stay paired with readScanTargets and goValueFromProtoReflect.
// All three describe the same column, and a cache id built from one must equal
// a cache id built from another. A type missing from the list falls to
// `new(string)`, which fails two ways:
//
//   - timestamptz, date, boolean and bytea cannot be scanned into *string at
//     all. pgx refuses with "cannot scan timestamptz (OID 1184) in binary
//     format into *string", so every Create on an entity with such a primary
//     key dies at the RETURNING clause, with a driver error naming neither
//     the column nor the cause. checkPKPredicates advertises exactly those
//     types as valid primary keys.
//   - float4 and float8 do scan into *string, and pgx hands back Postgres's
//     text rendering: a double holding 1e6 reads back as "1000000" here while
//     the read path produces float64(1e6), which runtime.CompositeID renders
//     "1e+06". Two ids for one row, which is the stale-cache failure above.
func makeScanTargets(cols []columnMeta) []any {
	targets := make([]any, len(cols))
	for i, cm := range cols {
		switch cm.field.Type.Name {
		case "text", "varchar", "citext", "uuid", "numeric", "interval":
			targets[i] = new(string)
		case "bigint":
			targets[i] = new(int64)
		case "int", "smallint":
			targets[i] = new(int32)
		case "real":
			targets[i] = new(float32)
		case "double":
			targets[i] = new(float64)
		case "boolean":
			targets[i] = new(bool)
		case "timestamptz", "date":
			targets[i] = new(time.Time)
		case "bytea", "jsonb":
			targets[i] = new([]byte)
		default:
			// Still a string, because a type this build does not know is
			// most likely text-shaped and refusing here would take down an
			// entity that works. It is the paired default in readScanTargets
			// that keeps the two consistent; a type reaching here is a gap in
			// the list above, and TestScanTargetsMatchGoValueFromProto names
			// every type that must not.
			targets[i] = new(string)
		}
	}
	return targets
}

// readPKScanTargets dereferences scan pointers into []any for cache
// keys or subsequent GET queries.
func readPKScanTargets(meta *entityMeta, targets []any) []any {
	return readScanTargets(meta.pkCols, targets)
}

// readScanTargets dereferences the pointers makeScanTargets produced.
func readScanTargets(cols []columnMeta, targets []any) []any {
	out := make([]any, len(targets))
	for i, cm := range cols {
		switch cm.field.Type.Name {
		case "text", "varchar", "citext", "uuid", "numeric", "interval":
			out[i] = *(targets[i].(*string))
		case "bigint":
			out[i] = *(targets[i].(*int64))
		case "int", "smallint":
			out[i] = *(targets[i].(*int32))
		case "real":
			out[i] = *(targets[i].(*float32))
		case "double":
			out[i] = *(targets[i].(*float64))
		case "boolean":
			out[i] = *(targets[i].(*bool))
		case "timestamptz", "date":
			out[i] = *(targets[i].(*time.Time))
		case "bytea", "jsonb":
			out[i] = *(targets[i].(*[]byte))
		default:
			out[i] = *(targets[i].(*string))
		}
	}
	return out
}

// updateMaskPaths reads update_mask.paths off an Update request.
//
// The mask is a nested dynamic message, since dynamicpb builds nested
// messages from the descriptor too; a *fieldmaskpb.FieldMask assertion on
// it fails. Nil when the mask is absent.
func updateMaskPaths(req *dynamicpb.Message) []string {
	fd := req.Descriptor().Fields().ByName("update_mask")
	if fd == nil || !req.Has(fd) {
		return nil
	}
	mask := req.Get(fd).Message()
	pathsFD := mask.Descriptor().Fields().ByName("paths")
	if pathsFD == nil {
		return nil
	}
	list := mask.Get(pathsFD).List()
	paths := make([]string, 0, list.Len())
	for i := 0; i < list.Len(); i++ {
		paths = append(paths, list.Get(i).String())
	}
	return paths
}
