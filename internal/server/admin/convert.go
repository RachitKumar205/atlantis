package admin

import (
	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
	"github.com/rachitkumar205/atlantis/internal/introspect"
)

// Wire conversion for the schema-lifecycle RPCs.
//
// PlanClass is the one field that changes representation rather than just
// container. It was a Go string type whose values ("additive",
// "backfill_required", …) went on the wire verbatim; the proto models it as an
// enum. The database is unaffected: schema_versions.plan_class is written from
// codegen.ChangeClass.String(), not from this type, so the TEXT column keeps
// exactly the values it always held. Only the wire representation moves, and
// the JSON shim maps the enum back to the original strings.

func planClassToPB(c ClassName) adminpb.PlanClass {
	switch c {
	case ClassAdditive:
		return adminpb.PlanClass_PLAN_CLASS_ADDITIVE
	case ClassBackfill:
		return adminpb.PlanClass_PLAN_CLASS_BACKFILL_REQUIRED
	case ClassBreaking:
		return adminpb.PlanClass_PLAN_CLASS_CROSS_CALLER_BREAKING
	case ClassUnclean:
		return adminpb.PlanClass_PLAN_CLASS_UNPARSEABLE
	}
	return adminpb.PlanClass_PLAN_CLASS_UNSPECIFIED
}

// planClassFromPB maps UNSPECIFIED to the empty ClassName rather than to
// "unparseable".
//
// UNSPECIFIED is unreachable today: codegen.Diff.HighestClass is total over
// three values and translateClass absorbs anything else into ClassUnclean, so
// no production path produces it. The mapping is defensive against the obvious
// future edit — fusing translateClass into this function and going from
// codegen.ChangeClass straight to the enum. Do that, and a newly added
// ChangeClass lands on UNSPECIFIED, then on "" here, and PlanResponse.Class
// has no omitempty, so `"class":""` ships. Both consumers hard-fail on it
// (cmd/tide/plan.go and apply.go print "unknown plan class" and exit 3), which
// is the right outcome: loud, not a silent misclassification.
//
// If the two are ever fused, translateClass's ClassUnclean default is the
// load-bearing part — the fused version must land on PLAN_CLASS_UNPARSEABLE,
// not UNSPECIFIED.
func planClassFromPB(c adminpb.PlanClass) ClassName {
	switch c {
	case adminpb.PlanClass_PLAN_CLASS_ADDITIVE:
		return ClassAdditive
	case adminpb.PlanClass_PLAN_CLASS_BACKFILL_REQUIRED:
		return ClassBackfill
	case adminpb.PlanClass_PLAN_CLASS_CROSS_CALLER_BREAKING:
		return ClassBreaking
	case adminpb.PlanClass_PLAN_CLASS_UNPARSEABLE:
		return ClassUnclean
	}
	return ""
}

func impactToPB(in []ImpactEntry) []*adminpb.ImpactEntry {
	if len(in) == 0 {
		return nil
	}
	out := make([]*adminpb.ImpactEntry, 0, len(in))
	for _, e := range in {
		out = append(out, &adminpb.ImpactEntry{Caller: e.Caller, Affected: e.Affected, Detail: e.Detail})
	}
	return out
}

func impactFromPB(in []*adminpb.ImpactEntry) []ImpactEntry {
	if len(in) == 0 {
		return nil
	}
	out := make([]ImpactEntry, 0, len(in))
	for _, e := range in {
		out = append(out, ImpactEntry{Caller: e.GetCaller(), Affected: e.GetAffected(), Detail: e.GetDetail()})
	}
	return out
}

func extensionsToPB(in []extensionStatus) []*adminpb.ExtensionStatus {
	if len(in) == 0 {
		return nil
	}
	out := make([]*adminpb.ExtensionStatus, 0, len(in))
	for _, e := range in {
		out = append(out, &adminpb.ExtensionStatus{
			Name: e.Name, Trigger: e.Trigger, Action: e.Action, InstallHint: e.InstallHint,
		})
	}
	return out
}

func extensionsFromPB(in []*adminpb.ExtensionStatus) []extensionStatus {
	if len(in) == 0 {
		return nil
	}
	out := make([]extensionStatus, 0, len(in))
	for _, e := range in {
		out = append(out, extensionStatus{
			Name: e.GetName(), Trigger: e.GetTrigger(), Action: e.GetAction(), InstallHint: e.GetInstallHint(),
		})
	}
	return out
}

func backfillFieldsToPB(in []BackfillFieldRef) []*adminpb.BackfillFieldRef {
	if len(in) == 0 {
		return nil
	}
	out := make([]*adminpb.BackfillFieldRef, 0, len(in))
	for _, f := range in {
		out = append(out, &adminpb.BackfillFieldRef{
			EntityId: f.EntityID, Field: f.Field, Expression: f.Expression,
			PkColumn: f.PKColumn, TableName: f.TableName,
		})
	}
	return out
}

func backfillFieldsFromPB(in []*adminpb.BackfillFieldRef) []BackfillFieldRef {
	if len(in) == 0 {
		return nil
	}
	out := make([]BackfillFieldRef, 0, len(in))
	for _, f := range in {
		out = append(out, BackfillFieldRef{
			EntityID: f.GetEntityId(), Field: f.GetField(), Expression: f.GetExpression(),
			PKColumn: f.GetPkColumn(), TableName: f.GetTableName(),
		})
	}
	return out
}

// The three drift kinds come from internal/introspect. They cross the wire so
// `tide plan --format=json` and the console can render the same remediation
// text the CLI prints.

func indexDriftToPB(in []introspect.UniqueIndexDrift) []*adminpb.UniqueIndexDrift {
	if len(in) == 0 {
		return nil
	}
	out := make([]*adminpb.UniqueIndexDrift, 0, len(in))
	for _, d := range in {
		out = append(out, &adminpb.UniqueIndexDrift{
			EntityId: d.EntityID, Schema: d.Schema, Table: d.Table,
			IndexName: d.IndexName, Columns: d.Columns,
			Partial: d.Partial, Predicate: d.Predicate,
		})
	}
	return out
}

func indexDriftFromPB(in []*adminpb.UniqueIndexDrift) []introspect.UniqueIndexDrift {
	if len(in) == 0 {
		return nil
	}
	out := make([]introspect.UniqueIndexDrift, 0, len(in))
	for _, d := range in {
		out = append(out, introspect.UniqueIndexDrift{
			EntityID: d.GetEntityId(), Schema: d.GetSchema(), Table: d.GetTable(),
			IndexName: d.GetIndexName(), Columns: d.GetColumns(),
			Partial: d.GetPartial(), Predicate: d.GetPredicate(),
		})
	}
	return out
}

func checkDriftKindToPB(k introspect.CheckDriftKind) adminpb.CheckDriftKind {
	switch k {
	case introspect.CheckDeclaredNotEnforced:
		return adminpb.CheckDriftKind_CHECK_DRIFT_KIND_DECLARED_NOT_ENFORCED
	case introspect.CheckLiveNotDeclared:
		return adminpb.CheckDriftKind_CHECK_DRIFT_KIND_LIVE_NOT_DECLARED
	}
	return adminpb.CheckDriftKind_CHECK_DRIFT_KIND_UNSPECIFIED
}

func checkDriftKindFromPB(k adminpb.CheckDriftKind) introspect.CheckDriftKind {
	switch k {
	case adminpb.CheckDriftKind_CHECK_DRIFT_KIND_DECLARED_NOT_ENFORCED:
		return introspect.CheckDeclaredNotEnforced
	case adminpb.CheckDriftKind_CHECK_DRIFT_KIND_LIVE_NOT_DECLARED:
		return introspect.CheckLiveNotDeclared
	}
	return ""
}

func checkDriftToPB(in []introspect.CheckConstraintDrift) []*adminpb.CheckConstraintDrift {
	if len(in) == 0 {
		return nil
	}
	out := make([]*adminpb.CheckConstraintDrift, 0, len(in))
	for _, d := range in {
		out = append(out, &adminpb.CheckConstraintDrift{
			Kind: checkDriftKindToPB(d.Kind), EntityId: d.EntityID,
			Schema: d.Schema, Table: d.Table, ConstraintName: d.ConstraintName,
			Declared: d.Declared, Definition: d.Definition,
		})
	}
	return out
}

func checkDriftFromPB(in []*adminpb.CheckConstraintDrift) []introspect.CheckConstraintDrift {
	if len(in) == 0 {
		return nil
	}
	out := make([]introspect.CheckConstraintDrift, 0, len(in))
	for _, d := range in {
		out = append(out, introspect.CheckConstraintDrift{
			Kind: checkDriftKindFromPB(d.GetKind()), EntityID: d.GetEntityId(),
			Schema: d.GetSchema(), Table: d.GetTable(), ConstraintName: d.GetConstraintName(),
			Declared: d.GetDeclared(), Definition: d.GetDefinition(),
		})
	}
	return out
}

func columnDriftToPB(in []introspect.ColumnTypeDrift) []*adminpb.ColumnTypeDrift {
	if len(in) == 0 {
		return nil
	}
	out := make([]*adminpb.ColumnTypeDrift, 0, len(in))
	for _, d := range in {
		out = append(out, &adminpb.ColumnTypeDrift{
			EntityId: d.EntityID, Schema: d.Schema, Table: d.Table,
			Column: d.Column, Declared: d.Declared, Live: d.Live,
		})
	}
	return out
}

func columnDriftFromPB(in []*adminpb.ColumnTypeDrift) []introspect.ColumnTypeDrift {
	if len(in) == 0 {
		return nil
	}
	out := make([]introspect.ColumnTypeDrift, 0, len(in))
	for _, d := range in {
		out = append(out, introspect.ColumnTypeDrift{
			EntityID: d.GetEntityId(), Schema: d.GetSchema(), Table: d.GetTable(),
			Column: d.GetColumn(), Declared: d.GetDeclared(), Live: d.GetLive(),
		})
	}
	return out
}
