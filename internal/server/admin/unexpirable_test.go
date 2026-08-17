package admin

import (
	"strings"
	"testing"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// A declaration whose expiry cannot run is refused, and one that can is not.
//
// The pairing is the point. A guard that refused everything partitioned would
// also pass a test that only checked the refusal, and it would block the very
// shape #50 exists to enable.
func TestUnexpirableEntities(t *testing.T) {
	cases := []struct {
		name   string
		entity dsl.Entity
		want   bool
	}{
		{
			// The defect: the sweeper binds no tenant, so its DELETE matches
			// nothing and succeeds.
			name: "partitioned with a ttl and no chunk dropping",
			entity: dsl.Entity{
				Name: "Session", Namespace: "app",
				TtlField: "expires_at", PartitionField: "tenant",
			},
			want: true,
		},
		{
			// The shape this whole change exists to make work.
			name: "partitioned hypertable whose ttl IS the time dimension",
			entity: dsl.Entity{
				Name: "Event", Namespace: "app",
				Kind:      dsl.EntityKindHypertable,
				TimeField: "occurred_at", TtlField: "occurred_at",
				PartitionField: "tenant",
			},
			want: false,
		},
		{
			// A hypertable whose ttl names a DIFFERENT column cannot chunk-drop:
			// chunks are selected by the time dimension, so dropping one could
			// remove rows whose ttl has not passed. Partitioned, so it is
			// refused rather than swept.
			name: "partitioned hypertable whose ttl is not the time dimension",
			entity: dsl.Entity{
				Name: "Event", Namespace: "app",
				Kind:      dsl.EntityKindHypertable,
				TimeField: "occurred_at", TtlField: "purge_after",
				PartitionField: "tenant",
			},
			want: true,
		},
		{
			// Unpartitioned: the sweeper sees every row, the DELETE works.
			name: "ttl without partitioning",
			entity: dsl.Entity{
				Name: "Session", Namespace: "app", TtlField: "expires_at",
			},
			want: false,
		},
		{
			// Partitioned but declares no expiry — nothing is promised, so
			// nothing is broken.
			name: "partitioned without a ttl",
			entity: dsl.Entity{
				Name: "Order", Namespace: "app", PartitionField: "tenant",
			},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ir := &dsl.IR{Entities: []dsl.Entity{tc.entity}}
			got := unexpirableEntities(ir)
			if tc.want && len(got) == 0 {
				t.Errorf("accepted, but the sweeper cannot expire this entity — " +
					"apply would record a retention rule atlantis silently does " +
					"not honour")
			}
			if !tc.want && len(got) != 0 {
				t.Errorf("refused %v, but this entity's expiry can run. Refusing it "+
					"blocks a schema that works", got)
			}
		})
	}
}

// The refusal has to name every way out, because which one is right is the
// schema author's call.
func TestUnexpirableErrorNamesEveryFix(t *testing.T) {
	err := unexpirableExpiryError([]string{"app.Session"})
	msg := err.Error()

	if !strings.Contains(msg, "app.Session") {
		t.Errorf("the error does not name the offending entity:\n%s", msg)
	}
	for _, want := range []string{"hypertable", "partition by", "caller"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error does not mention %q, so an author hitting it has "+
				"one fewer option than exists:\n%s", want, msg)
		}
	}
	// The correctness condition on the hypertable route. Without it an author
	// declares a hypertable on the wrong column and silently starts dropping
	// unexpired rows — a worse outcome than the refusal they were fixing.
	if !strings.Contains(msg, "time dimension") {
		t.Errorf("the error suggests a hypertable without saying the ttl_field "+
			"must BE the time dimension:\n%s", msg)
	}
}
