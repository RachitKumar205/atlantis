package admin

import (
	"fmt"
	"sort"
	"strings"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/schema"
)

// unexpirableEntities lists entities declaring expiry the platform cannot
// perform, in a stable order.
//
// Unexpirable means schema.ExpiryFor returns ExpiryUnreachable. The sweeper
// binds no tenant, so under FORCE ROW LEVEL SECURITY its DELETE matches nothing
// and succeeds.
//
// Refused, with no override, unlike the neighbouring apply checks.
func unexpirableEntities(ir *dsl.IR) []string {
	if ir == nil {
		return nil
	}
	var out []string
	for i := range ir.Entities {
		e := &ir.Entities[i]
		if schema.ExpiryFor(e) == schema.ExpiryUnreachable {
			out = append(out, e.ID())
		}
	}
	sort.Strings(out)
	return out
}

// unexpirableExpiryError renders the refusal, naming every fix.
//
// All three are listed because which one is right is the author's call and
// depends on what the data is for: chunk-dropping changes the storage layout,
// dropping `partition by` changes the isolation model, and moving expiry to the
// caller changes who is accountable for it. An error that named only the first
// would push every schema toward hypertables regardless of fit.
func unexpirableExpiryError(entities []string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "apply refused: %d entit%s declare expiry that cannot run.\n\n",
		len(entities), plural(len(entities), "y", "ies"))

	for _, id := range entities {
		fmt.Fprintf(&b, "  %s — has both `ttl_field` and `partition by`\n", id)
	}

	b.WriteString("\nThe TTL sweeper has no request behind it, so it binds no tenant. " +
		"Row-level security still applies to its DELETE, which therefore matches " +
		"nothing and succeeds — expired rows would accumulate forever with no " +
		"error. atlantis refuses to record a retention rule it will not honour.\n")

	b.WriteString("\nThree ways forward:\n")
	b.WriteString("  1. Declare the entity a `hypertable` on its ttl_field. Expiry then " +
		"drops whole chunks, which is DDL and not filtered by the tenant policy. " +
		"The ttl_field must BE the time dimension — chunks are selected by time, " +
		"so a different column could drop rows that have not expired.\n")
	b.WriteString("  2. Remove `partition by`. The sweeper can then see every row.\n")
	b.WriteString("  3. Remove `ttl_field` and expire the rows from the caller, which " +
		"binds a tenant per request. Expiry becomes the caller's responsibility, " +
		"and nothing on the platform will report it if it stops.\n")

	return fmt.Errorf("%s", b.String())
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
