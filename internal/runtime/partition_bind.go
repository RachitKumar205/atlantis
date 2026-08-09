package runtime

import (
	"context"
	"fmt"
)

// BindPartition hands the caller's tenant to PostgreSQL for the life of one
// transaction, so that row-level security enforces it.
//
// This is the join between the two halves of `partition by`. Codegen emits the
// policy — `<col> = atlantis.current_partition()` — and migration 0021 supplies
// the discriminator. Neither does anything until something calls this, and
// until it did, a partitioned entity returned zero rows to any role RLS applies
// to: the correct failure direction, and not a working feature.
//
// # Why the database rather than a predicate
//
// The obvious implementation is to append `AND tenant = $n` to every generated
// query. That is what the previous design did, and it is why the guarantee was
// missing: reads, writes and caller-authored SQL are three separate surfaces,
// and a handler added without the predicate leaks silently. Custom query bodies
// are opaque author text with nowhere to inject. Delegating to RLS means the
// filter applies to statements atlantis never sees.
//
// # What is trusted, and what is not
//
// The tenant is asserted by the calling service, which has already
// authenticated the end user and is the only party that knows whose request
// this is. atlantis does not attempt to derive it or to second-guess it — a
// caller that wanted another tenant's rows could query its own database.
//
// The guarantee is narrower and more useful than "callers cannot lie": once a
// tenant is asserted for a request, every statement in that transaction is
// confined to it, including SQL atlantis did not generate and did not inspect.
// The failure this prevents is the accidental one — a forgotten predicate, a
// new handler, a custom query — which is the failure that actually happens.
//
// # Fails closed
//
// With no tenant in context this returns ErrNoCallerPartition and the caller
// must abort. It must not fall through to an unbound transaction: an unset
// discriminator makes `current_partition()` NULL, and while that matches no
// rows for a restricted role, it silently returns everything to a role that
// bypasses RLS. Refusing is the only behaviour that is safe under both.
func BindPartition(ctx context.Context, tx Tx) error {
	v, err := CallerPartition(ctx)
	if err != nil {
		return err
	}
	s, ok := partitionString(v)
	if !ok {
		return fmt.Errorf("atlantis: caller partition has unusable type %T", v)
	}
	if s == "" {
		// set_partition rejects this too, but the message from here names the
		// layer that produced it.
		return fmt.Errorf("atlantis: caller partition is empty")
	}
	// Parameterised, not interpolated. The value comes from request metadata
	// and reaches a SECURITY DEFINER function.
	if _, err := tx.Exec(ctx, `SELECT atlantis.set_partition($1)`, s); err != nil {
		return fmt.Errorf("atlantis: bind caller partition: %w", err)
	}
	return nil
}

// partitionString renders a partition value as the text the discriminator
// stores. The column may be uuid or bigint — the policy casts the function
// side to match — but the discriminator itself is always text.
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
