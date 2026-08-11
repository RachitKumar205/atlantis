package runtime

import (
	"context"
	"fmt"
)

// BindPartition hands the caller's tenant to PostgreSQL for the life of one
// transaction, so that row-level security enforces it.
//
// This is the join between the two halves of `partition by`. Codegen emits the
// policy — `<col> = atlantis.current_partition()` — and migration 0024 supplies
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
// # Why the discriminator is a run-time parameter
//
// atlantis.set_partition writes atlantis.tenant transaction-locally, and every
// policy compares against it (migration 0024). A custom GUC is PGC_USERSET, so
// SQL that reaches the same transaction could reassign it — PostgreSQL offers
// no lock, and REVOKE SET ON PARAMETER does not create so much as an ACL row
// for a placeholder GUC. Two things close that, and this comment is the only
// place they appear together:
//
//   - sqlvalidate rejects set_config() and set_partition() in every
//     caller-authored SQL surface — query bodies, procedure steps, CHECK
//     expressions, partial-index predicates — by an exhaustive reflection walk
//     rather than a hand-listed one. Its statement gate already refused a bare
//     SET. The first version covered only query and procedure bodies, and a
//     CHECK expression walked straight through it.
//   - internal/storage/pg clears the parameter as each connection is opened,
//     so a value cannot arrive from a server default, a role default, or a
//     pooler handing back somebody else's backend.
//
// Removing either one reopens a cross-tenant read.
//
// set_partition's refusal to bind twice is NOT a third defence and must not be
// cited as one: set_config writes the parameter without going through the
// setter, so the guard never sees it. It catches the accidental double bind,
// which is a real defect and worth an error, and nothing more.
//
// The design this replaced — a locked table plus a SECURITY DEFINER setter —
// needed neither of the two, and cost a transaction ID per request: measured at
// 100 per 100 reads, which reaches the wraparound refusal in under two days at
// the throughput the same benchmark sustained. Migration 0024 carries the
// numbers and the arithmetic.
//
// # Fails closed
//
// With no tenant in context this returns ErrNoCallerPartition and the caller
// must abort. It must not fall through to an unbound transaction: an unset
// discriminator makes `current_partition()` NULL, and while that matches no
// rows for a restricted role, it silently returns everything to a role that
// bypasses RLS. Refusing is the only behaviour that is safe under both.
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

// partitionString renders a partition value as the text the discriminator
// stores. The column may be uuid or bigint — the policy casts the function
// side to match — but the discriminator itself is always text.
// PartitionKey returns the caller's tenant rendered EXACTLY as BindPartition
// renders it for set_partition.
//
// The code-generated server needs the tenant as a string in two places: the
// query-result cache key, and the defence-in-depth predicate. Rendering it
// there independently would be a second implementation of this conversion, and
// two renderings of one tenant is a permanent cache miss — every request
// stores an entry under a key the next request does not compute.
//
// It is also what makes the emitted code compile. The emitter passed the `any`
// from CallerPartition straight into queryresult.Hash, whose parameter is a
// string, so every generated server for a partitioned entity failed to build.
// Nothing in this repository caught it: the emitter tests only PARSE the output
// and no generated server is compiled anywhere in CI.
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
