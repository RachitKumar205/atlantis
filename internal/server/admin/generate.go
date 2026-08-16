package admin

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/dsl/atlemit"
	"github.com/rachitkumar205/atlantis/internal/introspect"
)

// GenerateSchema writes .atl describing tables no declaration mentions.
//
// # The onboarding problem this solves
//
// Introspection is declaration-driven: loadExistingTables filters pg_class to
// the tables the declaration already names. So adopting a database required
// .atl files describing every table in it — which is precisely what somebody
// adopting a legacy database does not have. The first step of onboarding was
// "hand-write fifty declarations, then we will check them".
//
// # How it works, and why so little of it is new
//
//	discover tables → stub entity per table → introspect → emit .atl
//
// Only the ends are new. FromPostgres fills a stub in because its column loop
// runs over live columns the declaration does not name, and fieldType already
// maps Postgres types to .atl types. The middle was built for adopt.
//
// # Read-only
//
// Same READ ONLY transaction as InspectSchema, for the same reason: this shares
// a path with adopt, and the guarantee should be one Postgres enforces rather
// than one a reviewer has to notice.
func (s *Service) GenerateSchema(ctx context.Context, req *adminpb.GenerateSchemaRequest) (*adminpb.GenerateSchemaResponse, error) {
	ns := req.GetNamespace()
	if ns == "" {
		return nil, errors.New("admin: namespace is required — it becomes part of every " +
			"generated entity's ID, so the server will not choose one")
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// Whatever is already declared, so its tables are not offered again.
	// Absent submissions this is nil, which DiscoverTables reads as "nothing
	// is declared yet" — the first run against a database atlantis has never
	// seen.
	var declaredIR *dsl.IR
	if subs := callerSubmissionsFromPB(req.GetSubmissions()); len(subs) > 0 {
		cmp, err := s.compareToLive(ctx, tx, subs)
		if err != nil {
			return nil, err
		}
		declaredIR = cmp.DeclaredIR
	}

	found, err := introspect.DiscoverTables(ctx, tx, declaredIR, req.GetSchemas())
	if err != nil {
		return nil, err
	}
	if len(found) == 0 {
		return &adminpb.GenerateSchemaResponse{}, nil
	}

	// One stub per discovered table, all introspected together. Together
	// rather than one at a time because a foreign key can only resolve to an
	// entity in the same IR — introspecting singly would drop every reference
	// between two discovered tables, which on a normalised schema is most of
	// them.
	stubs := &dsl.IR{Entities: make([]dsl.Entity, 0, len(found))}
	nameFor := make(map[string]string, len(found))
	var skipped []string
	seen := map[string]bool{}
	for _, d := range found {
		name := d.SuggestedName()
		if name == "" {
			skipped = append(skipped, fmt.Sprintf("%s: no entity name could be derived from the table name", d.Qualified()))
			continue
		}
		// Two tables in different Postgres schemas can share a name, and both
		// would land in the one atlantis namespace this request names. The
		// second is reported rather than silently overwriting the first.
		if seen[name] {
			skipped = append(skipped, fmt.Sprintf(
				"%s: entity name %q is already taken by another discovered table — "+
					"generate it separately into its own namespace", d.Qualified(), name))
			continue
		}
		seen[name] = true
		nameFor[ns+"."+name] = d.Qualified()
		stubs.Entities = append(stubs.Entities, dsl.Entity{
			Name: name, Namespace: ns, TableName: d.Qualified(),
		})
	}
	if len(stubs.Entities) == 0 {
		return &adminpb.GenerateSchemaResponse{Skipped: skipped}, nil
	}

	filled, _, warnings, err := introspect.FromPostgres(ctx, tx, stubs)
	if err != nil {
		return nil, fmt.Errorf("introspect discovered tables: %w", err)
	}

	out := make([]*adminpb.GeneratedEntity, 0, len(filled.Entities))
	for i := range filled.Entities {
		e := &filled.Entities[i]
		table := nameFor[e.ID()]
		out = append(out, &adminpb.GeneratedEntity{
			Table:      table,
			EntityName: e.Name,
			Atl:        atlemit.Entity(e, table),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetTable() < out[j].GetTable() })

	return &adminpb.GenerateSchemaResponse{
		Entities: out,
		Skipped:  skipped,
		Warnings: warnings,
	}, nil
}
