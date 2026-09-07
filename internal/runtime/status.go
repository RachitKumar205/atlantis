package runtime

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ToStatus maps an error a handler returned to the gRPC status a caller can
// act on. An error that already carries a status is returned unchanged; nil
// is returned as nil. ErrNotFound maps to NotFound, a context error to
// DeadlineExceeded or Canceled, a *pgconn.PgError by SQLSTATE, and any other
// error to Internal.
//
// SQLSTATE classes, from the PostgreSQL error-code appendix:
//
//	23505 unique_violation           AlreadyExists
//	23503 foreign_key_violation      FailedPrecondition
//	23502 not_null_violation         InvalidArgument
//	23514 check_violation            InvalidArgument
//	22xxx data exception             InvalidArgument
//	40001 serialization_failure      Aborted
//	40P01 deadlock_detected          Aborted
//	57014 query_canceled             DeadlineExceeded
//	42501 insufficient_privilege     PermissionDenied
//	53xxx insufficient resources     ResourceExhausted
//	08xxx connection exception       Unavailable
//
// Anything else from Postgres is Internal, with the message kept: the SQLSTATE
// and the constraint name are what a caller reads back to atlantis, and the
// existing behaviour already exposed them.
func ToStatus(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	switch {
	case errors.Is(err, ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return status.Error(sqlStateCode(pgErr.Code), err.Error())
	}
	return status.Error(codes.Internal, err.Error())
}

func sqlStateCode(sqlstate string) codes.Code {
	switch sqlstate {
	case "23505":
		return codes.AlreadyExists
	case "23503":
		return codes.FailedPrecondition
	case "23502", "23514":
		return codes.InvalidArgument
	case "40001", "40P01":
		return codes.Aborted
	case "57014":
		return codes.DeadlineExceeded
	case "42501":
		return codes.PermissionDenied
	}
	if len(sqlstate) >= 2 {
		switch sqlstate[:2] {
		case "22":
			return codes.InvalidArgument
		case "53":
			return codes.ResourceExhausted
		case "08":
			return codes.Unavailable
		}
	}
	return codes.Internal
}
