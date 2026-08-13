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
// nothing else changes.

func planClassToPB(c ClassName) adminpb.PlanClass {
	switch c {
	case ClassAdditive:
		return adminpb.PlanClass_PLAN_CLASS_ADDITIVE
	case ClassBackfill:
		return adminpb.PlanClass_PLAN_CLASS_BACKFILL_REQUIRED
	case ClassBreaking:
		return adminpb.PlanClass_PLAN_CLASS_CROSS_CALLER_BREAKING
	case ClassDestructive:
		return adminpb.PlanClass_PLAN_CLASS_DESTRUCTIVE
	case ClassUnclean:
		return adminpb.PlanClass_PLAN_CLASS_UNPARSEABLE
	}
	// The SECOND hop. translateClass maps codegen.ChangeClass to ClassName and
	// this maps ClassName to the wire enum; a class needs an arm in both.
	// Adding only the first moved destructive from UNPARSEABLE to UNSPECIFIED —
	// still exit 3 at the CLI, just a different wrong message.
	return adminpb.PlanClass_PLAN_CLASS_UNSPECIFIED
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

func checkDriftKindToPB(k introspect.CheckDriftKind) adminpb.CheckDriftKind {
	switch k {
	case introspect.CheckDeclaredNotEnforced:
		return adminpb.CheckDriftKind_CHECK_DRIFT_KIND_DECLARED_NOT_ENFORCED
	case introspect.CheckLiveNotDeclared:
		return adminpb.CheckDriftKind_CHECK_DRIFT_KIND_LIVE_NOT_DECLARED
	}
	return adminpb.CheckDriftKind_CHECK_DRIFT_KIND_UNSPECIFIED
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
