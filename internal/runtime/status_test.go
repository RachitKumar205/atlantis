package runtime

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestToStatus(t *testing.T) {
	pg := func(sqlstate string) error {
		return fmt.Errorf("exec: %w", &pgconn.PgError{Code: sqlstate, Message: "boom"})
	}
	cases := []struct {
		name string
		err  error
		want codes.Code
	}{
		{"not found, wrapped", fmt.Errorf("get: %w", ErrNotFound), codes.NotFound},
		{"deadline", context.DeadlineExceeded, codes.DeadlineExceeded},
		{"cancelled", context.Canceled, codes.Canceled},
		{"unique violation", pg("23505"), codes.AlreadyExists},
		{"foreign key", pg("23503"), codes.FailedPrecondition},
		{"not null", pg("23502"), codes.InvalidArgument},
		{"check", pg("23514"), codes.InvalidArgument},
		{"invalid text representation", pg("22P02"), codes.InvalidArgument},
		{"serialization failure", pg("40001"), codes.Aborted},
		{"deadlock", pg("40P01"), codes.Aborted},
		{"query cancelled", pg("57014"), codes.DeadlineExceeded},
		{"insufficient privilege", pg("42501"), codes.PermissionDenied},
		{"too many connections", pg("53300"), codes.ResourceExhausted},
		{"connection failure", pg("08006"), codes.Unavailable},
		{"other postgres error", pg("XX000"), codes.Internal},
		{"plain error", errors.New("something"), codes.Internal},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ToStatus(c.err)
			st, ok := status.FromError(got)
			if !ok {
				t.Fatalf("ToStatus returned %T, not a status", got)
			}
			if st.Code() != c.want {
				t.Errorf("code = %v, want %v", st.Code(), c.want)
			}
			// The message survives: it is what a caller reads back to atlantis.
			if st.Message() == "" {
				t.Errorf("message was dropped")
			}
		})
	}
}

func TestToStatusLeavesAStatusAlone(t *testing.T) {
	in := status.Error(codes.Unimplemented, "fields are not served")
	if got := ToStatus(in); got != in {
		t.Errorf("a status error was rewritten to %v", got)
	}
	if got := ToStatus(nil); got != nil {
		t.Errorf("nil became %v", got)
	}
}

// TestToStatusKeepsTheSQLState pins what the message carries: a caller who
// reads "SQLSTATE 23505" out of an AlreadyExists is reading the same string
// the server logged.
func TestToStatusKeepsTheSQLState(t *testing.T) {
	err := &pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint \"library_author_name_key\""}
	st, _ := status.FromError(ToStatus(err))
	if st.Message() != err.Error() {
		t.Errorf("message = %q, want the PgError's own %q", st.Message(), err.Error())
	}
}
