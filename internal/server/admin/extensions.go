package admin

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// Extension detection + auto-enable.
//
// atlantis can't reach into the Postgres host's filesystem to apt-install
// extension binaries. What it CAN do is enable an already-installed
// extension via `CREATE EXTENSION IF NOT EXISTS foo;` inside the apply
// transaction, and tell the operator clearly what to install at the OS
// level when an extension is required by the schema but missing on disk.
//
// Triggers (DSL → extension):
//   vector(N) column         → vector   (pgvector)
//   Entity.Kind=hypertable   → timescaledb
//   citext column            → citext   (postgresql-contrib)
//
// At apply time the server walks the new IR, cross-references against
// pg_available_extensions and pg_extension, auto-enables what's available
// but not yet enabled, and refuses with a structured error when an
// extension is required but unavailable at OS level.

// extensionReq describes one extension the schema needs, with a
// human-readable trigger pointing back at the entity/field responsible.
type extensionReq struct {
	Name    string // pg_extension name
	Trigger string // first thing in the schema that needs it
}

// extensionStatus is what the server reports per required extension.
// Action is "ok" (already enabled), "enable" (atlantis will enable in
// the apply tx), or "missing" (refuse — operator must install at OS).
type extensionStatus struct {
	Name        string `json:"name"`
	Trigger     string `json:"trigger"`
	Action      string `json:"action"` // ok | enable | missing
	InstallHint string `json:"install_hint,omitempty"`
}

// requiredExtensions walks the IR and returns the set of extensions
// needed, with a stable iteration order (sorted by extension name) so
// log lines and plan responses are deterministic.
func requiredExtensions(ir *dsl.IR) []extensionReq {
	if ir == nil {
		return nil
	}
	seen := map[string]extensionReq{}
	note := func(name, trigger string) {
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = extensionReq{Name: name, Trigger: trigger}
	}
	for i := range ir.Entities {
		e := &ir.Entities[i]
		if e.Kind == dsl.EntityKindHypertable {
			note("timescaledb", fmt.Sprintf("entity %q is a hypertable", e.ID()))
		}
		for j := range e.Fields {
			f := &e.Fields[j]
			walkFieldType(&f.Type, e.ID(), f.Name, note)
		}
	}
	out := make([]extensionReq, 0, len(seen))
	for _, r := range seen {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// walkFieldType descends through Array/Elem so a hypothetical
// `[]vector(N)` would still trigger pgvector. Today the parser doesn't
// emit nested types like that, but defensive walking costs nothing.
func walkFieldType(t *dsl.FieldType, entityID, fieldName string, note func(name, trigger string)) {
	if t == nil {
		return
	}
	switch t.Name {
	case "vector":
		note("vector", fmt.Sprintf("entity %q field %q is vector(%d)", entityID, fieldName, t.VecDim))
	case "citext":
		note("citext", fmt.Sprintf("entity %q field %q is citext", entityID, fieldName))
	}
	if t.Array && t.Elem != nil {
		walkFieldType(t.Elem, entityID, fieldName, note)
	}
}

// extLookup is the subset of pgx Query semantics availableExtensions /
// installedExtensions need; both *pgxpool.Pool and pgx.Tx satisfy it
// so plan-time (pool, no tx) and apply-time (inside tx) share helpers.
type extLookup interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// availableExtensions returns the set of extensions present at the OS
// level (installable via CREATE EXTENSION). pg_available_extensions lists
// every extension whose control file is on disk; missing rows mean the
// .so isn't installed and CREATE EXTENSION would fail.
func availableExtensions(ctx context.Context, q extLookup) (map[string]bool, error) {
	rows, err := q.Query(ctx, "SELECT name FROM pg_available_extensions")
	if err != nil {
		return nil, fmt.Errorf("query pg_available_extensions: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// installedExtensions returns the set of extensions already enabled in
// the current database (pg_extension is per-database, not per-cluster).
func installedExtensions(ctx context.Context, q extLookup) (map[string]bool, error) {
	rows, err := q.Query(ctx, "SELECT extname FROM pg_extension")
	if err != nil {
		return nil, fmt.Errorf("query pg_extension: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// inspectExtensions is the read-only classify used at plan time. Returns
// nil + nil when the schema needs no extensions; the absence of a status
// list is informative to callers (no extension surface to surface).
func inspectExtensions(ctx context.Context, q extLookup, ir *dsl.IR) ([]extensionStatus, error) {
	required := requiredExtensions(ir)
	if len(required) == 0 {
		return nil, nil
	}
	available, err := availableExtensions(ctx, q)
	if err != nil {
		return nil, err
	}
	installed, err := installedExtensions(ctx, q)
	if err != nil {
		return nil, err
	}
	return classifyExtensions(required, installed, available), nil
}

// classifyExtensions cross-references the IR's required set against the
// live DB's installed + available sets, returning one extensionStatus
// per required extension. Pure function over the three sets so it's
// trivially testable.
func classifyExtensions(required []extensionReq, installed, available map[string]bool) []extensionStatus {
	out := make([]extensionStatus, 0, len(required))
	for _, r := range required {
		st := extensionStatus{Name: r.Name, Trigger: r.Trigger}
		switch {
		case installed[r.Name]:
			st.Action = "ok"
		case available[r.Name]:
			st.Action = "enable"
		default:
			st.Action = "missing"
			st.InstallHint = osInstallHint(r.Name)
		}
		out = append(out, st)
	}
	return out
}

// prepareExtensions is the apply-time orchestration: walk the IR, query
// the live DB, enable available-but-not-installed extensions inside the
// caller's transaction, and refuse with a structured error if any
// required extension is missing at OS level. Returns the list of
// extensions that were just enabled so callers can log them.
//
// Must run BEFORE the DDL exec — Postgres CREATE EXTENSION is itself
// transactional, so the enable + DDL commit atomically.
func prepareExtensions(ctx context.Context, tx pgx.Tx, ir *dsl.IR) ([]string, error) {
	statuses, err := inspectExtensions(ctx, tx, ir)
	if err != nil {
		return nil, err
	}
	if len(statuses) == 0 {
		return nil, nil
	}

	var missing []extensionStatus
	var toEnable []string
	for _, s := range statuses {
		switch s.Action {
		case "missing":
			missing = append(missing, s)
		case "enable":
			toEnable = append(toEnable, s.Name)
		}
	}
	if len(missing) > 0 {
		return nil, extensionsMissingError(missing)
	}
	for _, name := range toEnable {
		// CREATE EXTENSION takes an identifier, not a parameterized
		// value. extension names come from our own constant set, never
		// from caller input, so no injection surface.
		stmt := fmt.Sprintf(`CREATE EXTENSION IF NOT EXISTS %s`, quoteIdent(name))
		if _, err := tx.Exec(ctx, stmt); err != nil {
			return nil, fmt.Errorf("CREATE EXTENSION %s: %w (atlantis could not enable this extension despite pg_available_extensions listing it — the most common cause is the atlantis role lacking privilege; ask your DBA to run `%s` in the atlantis database, or check Postgres logs for the underlying error)",
				name, err, stmt)
		}
	}
	return toEnable, nil
}

// osInstallHint returns the apt-flavored command an operator runs at
// the Postgres host to make an extension installable. Other distros'
// package managers differ; the hint is descriptive, not a script.
func osInstallHint(name string) string {
	switch name {
	case "vector":
		return "install pgvector at the OS level (`apt install postgresql-15-pgvector` on Debian-family, or use a Postgres image that bundles it — Timescale's `timescaledb-ha:pg16-all` ships it, and most managed Postgres flavors support it on recent engine versions)"
	case "timescaledb":
		return "install timescaledb at the OS level — see https://docs.timescale.com/self-hosted/latest/install/ for distro-specific instructions"
	case "citext":
		return "install the postgres-contrib package at the OS level (`apt install postgresql-contrib-15`) — almost always shipped alongside the Postgres server package"
	default:
		return fmt.Sprintf("install the OS-level package providing the `%s` Postgres extension", name)
	}
}

// extensionsMissingError formats one human-readable error covering every
// missing extension at once. Apply RPC returns this verbatim; tide
// surfaces it in the plan-rejected message.
func extensionsMissingError(missing []extensionStatus) error {
	var b strings.Builder
	b.WriteString("schema requires extensions not available on this Postgres:\n")
	for _, m := range missing {
		fmt.Fprintf(&b, "  - %s — needed because %s\n", m.Name, m.Trigger)
		fmt.Fprintf(&b, "    %s\n", m.InstallHint)
	}
	b.WriteString("\natlantis will enable each extension automatically once the OS package is installed.")
	return errors.New(b.String())
}

// quoteIdent is a minimal identifier quoter for CREATE EXTENSION. The
// extension names come from a closed constant set so double-quoting is
// belt-and-braces, not a real defense.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
