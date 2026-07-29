package admin

import (
	"encoding/json"
	"testing"

	adminpb "github.com/rachitkumar205/atlantis/clients/go/pb/atlantis/admin/v1"
)

// The JSON path is what production serves while the protobuf migration is in
// flight, so a shim that drifts is a live regression rather than a latent one.
// These tests pin the emitted bytes, not the Go values: CallerInfo carries
// omitempty on three fields, so the question is which keys exist, and no
// assertion on a struct answers that.

func TestCallerInfosToJSON_EmptyMarshalsAsNull(t *testing.T) {
	// The pre-migration handler declared `var out []CallerInfo` and never
	// assigned it when the query returned nothing, so the field marshalled as
	// null. The console SPA reads this response; emitting [] instead would be
	// a silent shape change.
	got, err := json.Marshal(&GetCallersResponse{Callers: callerInfosToJSON(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"callers":null}` {
		t.Errorf("got %s, want {\"callers\":null}", got)
	}

	// An empty non-nil input must behave the same — the distinction is in the
	// helper, not in what the caller happens to pass.
	got, err = json.Marshal(&GetCallersResponse{Callers: callerInfosToJSON([]*adminpb.CallerInfo{})})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"callers":null}` {
		t.Errorf("empty slice: got %s, want {\"callers\":null}", got)
	}
}

func TestCallerInfosToJSON_OmitemptyKeysMatchPreMigrationShape(t *testing.T) {
	tests := []struct {
		name string
		in   *adminpb.CallerInfo
		want string
	}{
		{
			// A caller that has never applied: all three omitempty fields are
			// zero, so all three keys must be absent.
			name: "zero values omit their keys",
			in:   &adminpb.CallerInfo{Caller: "svc-a"},
			want: `{"callers":[{"caller":"svc-a","file_count":0,"registered":false,"can_mutate":false}]}`,
		},
		{
			name: "populated values emit every key",
			in: &adminpb.CallerInfo{
				Caller:        "svc-b",
				FileCount:     3,
				LastAppliedAt: "2026-07-29T00:00:00Z",
				SchemaVersion: 7,
				Registered:    true,
				CanMutate:     true,
				CertExpiresAt: "2026-10-27T00:00:00Z",
			},
			want: `{"callers":[{"caller":"svc-b","file_count":3,"last_applied_at":"2026-07-29T00:00:00Z","schema_version":7,"registered":true,"can_mutate":true,"cert_expires_at":"2026-10-27T00:00:00Z"}]}`,
		},
		{
			// schema_version is the one numeric omitempty field: version 0 is
			// not a real version, so its absence is correct, but a future
			// change to a pointer type would alter this and should fail here.
			name: "zero schema_version omits only that key",
			in: &adminpb.CallerInfo{
				Caller:        "svc-c",
				FileCount:     1,
				LastAppliedAt: "2026-07-29T00:00:00Z",
				Registered:    true,
			},
			want: `{"callers":[{"caller":"svc-c","file_count":1,"last_applied_at":"2026-07-29T00:00:00Z","registered":true,"can_mutate":false}]}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(&GetCallersResponse{
				Callers: callerInfosToJSON([]*adminpb.CallerInfo{tc.in}),
			})
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// TestCallerInfosToJSON_CopiesEveryField guards against a field being added to
// CallerInfo and silently left unmapped — the shim would compile and emit a
// zero value, which for an omitempty field means the key just disappears.
func TestCallerInfosToJSON_CopiesEveryField(t *testing.T) {
	in := &adminpb.CallerInfo{
		Caller:        "svc",
		FileCount:     9,
		LastAppliedAt: "t1",
		SchemaVersion: 4,
		Registered:    true,
		CanMutate:     true,
		CertExpiresAt: "t2",
	}
	got := callerInfosToJSON([]*adminpb.CallerInfo{in})[0]

	want := CallerInfo{
		Caller:        "svc",
		FileCount:     9,
		LastAppliedAt: "t1",
		SchemaVersion: 4,
		Registered:    true,
		CanMutate:     true,
		CertExpiresAt: "t2",
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}
