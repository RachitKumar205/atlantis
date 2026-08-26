package runtime

import (
	"context"
	"fmt"
)

// BindPartition hands the caller's tenant to PostgreSQL for the life of one
// transaction, so that row-level security enforces it.
//
// Codegen emits the policy `<col> = atlantis.current_partition()` and migration
// 0024 supplies the value; without this call a partitioned entity returns no
// rows to any role RLS applies to.
//
// Returns ErrNoCallerPartition when the context carries no tenant. Callers must
// abort rather than run unbound: current_partition() is NULL when unset, which
// matches no rows under RLS and every row under a role that bypasses it.
//
// atlantis.tenant is PGC_USERSET, so two things stop caller SQL reassigning it:
// sqlvalidate rejects set_config and set_partition in caller-authored surfaces,
// and internal/storage/pg clears the parameter on each new connection.
//
// set_partition's refusal to bind twice is not a third defence. set_config
// writes the parameter without going through the setter.
func BindPartition(ctx context.Context, tx Tx) error {
	s, err := PartitionKey(ctx)
	if err != nil {
		return err
	}
	if s == "" {
		// set_partition rejects this too, but the message from here names the
		// layer that produced it.
		return fmt.Errorf("atlantis: caller partition is empty")
	}
	// Parameterised, not interpolated. The value comes from request metadata and
	// is interpolated nowhere — set_partition is no longer SECURITY DEFINER
	// (it writes a parameter, not a privileged table), so the reason is ordinary
	// injection safety rather than privilege escalation.
	if _, err := tx.Exec(ctx, `SELECT atlantis.set_partition($1)`, s); err != nil {
		return fmt.Errorf("atlantis: bind caller partition: %w", err)
	}
	return nil
}

// PartitionKey returns the caller's tenant rendered exactly as BindPartition
// renders it for set_partition.
//
// The generated server needs the tenant as a string for the query-result cache
// key and for the defence-in-depth predicate. Two renderings of one tenant is a
// permanent cache miss: each request stores an entry under a key the next does
// not compute.
//
// Note that emitter tests only parse their output and no generated server is
// compiled in CI, so a type error here is not caught by this repository.
func PartitionKey(ctx context.Context) (string, error) {
	v, err := CallerPartition(ctx)
	if err != nil {
		return "", err
	}
	s, ok := partitionString(v)
	if !ok {
		return "", fmt.Errorf("atlantis: caller partition has unusable type %T", v)
	}
	return s, nil
}

// partitionString renders a partition value as the text the discriminator
// stores. The column may be uuid or bigint, and the policy casts the function
// side to match, but the discriminator itself is always text.
func partitionString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case fmt.Stringer:
		return t.String(), true
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return fmt.Sprintf("%d", t), true
	default:
		return "", false
	}
}
