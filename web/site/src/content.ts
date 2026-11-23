// The page's copy and its sample artifacts. index.astro and markdown.ts both
// render from here. The copy follows docs/STYLE.md.
//
// The CLI text follows cliout.Header/Row/Field (internal/cliout/cliout.go),
// cmd/tide/plan.go, cmd/tide/rehearse.go and cmd/tide/apply.go; the rehearsal
// error, violation key and remediation follow internal/server/admin/rehearse.go,
// and the approval message approvalRequired in internal/server/admin/approval.go.
// The SQL is codegen.EmitSQL's output without its comments.
//
// Counts, ids, hashes, timings and timestamps are sample values.

const schemaWithout = [
  "entity Customer in shop {",
  "  id         bigint primary serial",
  "  name       varchar(200) not null",
  "  email      varchar(255) not null",
  "  created_at timestamptz not null default now()",
  "}",
].join("\n");

// A removal the submitting caller owns, with no other caller reading it, is
// destructive (internal/codegen/diff.go classifyRemoval).
const planOutput = [
  "plan ───────────────────",
  "  plan_id  9f4c2a71e0b84d3a6c15f2e8a0d7b493",
  "  class    destructive",
  "  approval required (admin)",
  "           `tide apply` files the request; admin decides under Approvals",
  "",
  "breaking ───────────────",
  "  ✖  shop.Customer/legacy_notes: field removed (parked, reaped after the retention window)",
].join("\n");

// The file before the edit had `legacy_notes text` between email and
// created_at.
const diffOutput = [
  "diff --git a/schema/shop.atl b/schema/shop.atl",
  "index 4b1e2c9..a07d3f5 100644",
  "--- a/schema/shop.atl",
  "+++ b/schema/shop.atl",
  "@@ -2,6 +2,5 @@ entity Customer in shop {",
  "   id         bigint primary serial",
  "   name       varchar(200) not null",
  "   email      varchar(255) not null",
  "-  legacy_notes text",
  "   created_at timestamptz not null default now()",
  " }",
].join("\n");

// The destructive arm of cmdApply, then doApply after an admin approves. The
// first two blocks go to stderr.
//
// The request was filed at 17:04:31 on 15 November, which is why the hero's
// Approvals page shows it expiring 7 days later (defaultPlanTTL).
const applyOutput = [
  "tide: this change destroys data:",
  "   shop.Customer/legacy_notes: field removed (parked, reaped after the retention window)",
  "",
  "      Dropped objects are parked, not deleted, and reaped after the",
  "      retention window — `tide parked` shows what is held and until when.",
  "",
  "ℹ waiting up to 30m0s for a decision — admin: this change is destructive and needs approval from admin before it can apply. Plan 9f4c2a71e0b84d3a6c15f2e8a0d7b493: recorded and waiting for a decision.",
  "✔ applied at 2026-11-15T17:09:12Z",
  "  content  3b9e0c41a7d2",
].join("\n");

const passOutput = [
  "rehearsal ──────────────",
  "  ●  verdict                 pass",
  "  ·  timing                  clone 38412ms, execute 41ms",
  "",
  "The migration ran cleanly against a clone of the real data.",
].join("\n");

export const parkSql = [
  "SET LOCAL lock_timeout = '3000ms';",
  'ALTER TABLE "atlantis"."shop_customer" RENAME COLUMN "legacy_notes" TO "legacy_notes__parked";',
  'ALTER TABLE "atlantis"."shop_customer" ALTER COLUMN "legacy_notes__parked" DROP NOT NULL;',
  "INSERT INTO atlantis.parked_objects (kind, schema_name, object_name, parent_table, original_schema, original_name, reap_after) VALUES ('column', 'atlantis', 'legacy_notes__parked', 'shop_customer', 'atlantis', 'legacy_notes', now() + INTERVAL '30 days') ON CONFLICT (schema_name, coalesce(parent_table, ''), object_name) WHERE reaped_at IS NULL DO NOTHING;",
].join("\n");

export const parkSchema = schemaWithout;

// The verdict row's bullet is a faint `·`: tide rehearse passes the tone "bad",
// which cliout's rowBullet does not map.
export const session = [
  "$ grep email schema/shop.atl",
  "  email      varchar(255) not null unique",
  "$ tide rehearse",
  "rehearsal ──────────────",
  "  ·  verdict                 fail_data",
  '  ·  error                   SQLSTATE 23505: could not create unique index "shop_customer_email_key" (constraint shop_customer_email_key) on shop_customer',
  "  ·  violations              shop_customer.email duplicate: 312",
  '  ●  suggested               312 rows in shop_customer share 147 values of email, so the unique index cannot be built. Make the values distinct, then rehearse again. This query lists them: `SELECT "email", count(*) FROM "atlantis"."shop_customer" WHERE "email" IS NOT NULL GROUP BY "email" HAVING count(*) > 1`',
  "  ·  timing                  clone 38412ms, execute 214ms",
  "",
  "The real apply would fail the same way. Fix the schema or the data, then rehearse again.",
].join("\n");

export const hero = {
  approval: {
    title: "The Approvals page",
    text: "An agent removes a column that no other repository references. The change is destructive, so it waits for an admin.",
  },
};

export const loop = {
  heading: "Four steps between an agent's edit and production",
  lead: "The window below plays one change through them: an agent removes the `legacy_notes` column, which no other repository's schema references.",
  steps: [
    {
      title: "Edit the schema",
      text: "An agent changes a `.atl` file in your repository. Atlantis writes the migration SQL from the diff.",
      command: "git diff schema/shop.atl",
      lang: "diff",
      code: diffOutput,
    },
    {
      title: "Plan",
      text: "`tide plan` sorts the change into one of four classes and says whether it needs approval before it applies.",
      command: "tide plan",
      lang: "tideout",
      code: planOutput,
    },
    {
      title: "Rehearse",
      text: "`tide rehearse` runs the migration on a clone of your production data and reports what would fail.",
      command: "tide rehearse",
      lang: "tideout",
      code: passOutput,
    },
    {
      title: "Approve and apply",
      text: "By default, `tide apply` runs additive changes at once. Others wait for an admin to approve them with an authenticator code.",
      command: "tide apply --wait-for-approval=30m",
      lang: "tideout",
      code: applyOutput,
    },
  ],
};

// Each example is two lines of a diff of shop.atl. In the cross-caller example
// another repository's schema references `region`.
//
// The impact rows of `tide plan` (buildImpactReport) list every other
// repository and mark the ones that declare an entity the plan changes.
export const classes = {
  heading: "Changes that can fail, break code or delete data wait for an admin",
  lead: "The schema diff puts each change in one of four classes. By default, only the first applies without an admin.",
  groups: { applies: "Applies at once", waits: "Waits for an admin" },
  rows: [
    {
      tone: "sage",
      name: "Additive",
      diff: ["  email varchar(255)", "+ phone varchar(32)"],
      text: "New tables, nullable columns and indexes, or a looser constraint, such as dropping `not null`.",
    },
    {
      tone: "warn",
      name: "Backfill required",
      diff: ["- email varchar(255)", "+ email varchar(255) unique"],
      text: "Existing rows might fail it, as with a new `not null` or `unique`, or a narrower type.",
    },
    {
      tone: "coral",
      name: "Cross-caller breaking",
      diff: ["  email varchar(255)", "- region varchar(32)"],
      text: "Another repository's schema references what it changes. The plan marks each repository it affects.",
    },
    {
      tone: "coral",
      name: "Destructive",
      diff: ["  email varchar(255)", "- legacy_notes text"],
      text: "Removes a column or table that holds data. The data stays for 30 days, and declaring it again restores its rows.",
    },
  ],
};

// Rehearsal fails a change on any SQLSTATE in class 22 or 23
// (internal/server/admin/rehearse.go), which covers duplicates, NULLs and
// values too long for their column. Each point's pin is the start of the
// session line it annotates.
export const rehearsal = {
  heading: "Test changes against your production data before they ship",
  lead: "Test databases do not have your duplicates, empty fields and oversized values, so a change can pass every test and still fail in production. Atlantis runs the change on a copy of your real rows and tells your agent what would break.",
  points: [
    {
      pin: "$ tide rehearse",
      title: "Production does not change",
      text: "Atlantis copies your rows into a throwaway database, runs the change there and deletes the copy.",
    },
    {
      pin: "  ·  violations",
      title: "You see why it would fail",
      text: "In this run, 312 customers share 147 email addresses, so the new `unique` rule cannot be added.",
    },
    {
      pin: "  ●  suggested",
      title: "Your agent gets the rows to fix",
      text: "A query lists every duplicate. Your agent can clean them up and rehearse again.",
    },
  ],
};

// The SQL is codegen.EmitInitial's policy for this entity, with its one line
// wrapped. The service names are EmitProto's, and the client call is the
// Python client's from EmitPyClient.
//
// A partitioned entity refuses a request that names no tenant
// (runtime.BindPartition).
//
// `mark` is the tenant in each output, highlighted with `partition by tenant`
// in the schema.
export const api = {
  heading: "Give your tables a typed API without writing a server",
  lead: "Declare an entity in your schema. Atlantis creates the table, serves an API for it as soon as the change applies, and generates the typed client your services and agents call.",
  file: "schema/shop.atl",
  schema: [
    "entity Order in shop {",
    "  id         bigint primary serial",
    "  tenant     varchar(32) not null",
    "  total      numeric(10,2) not null",
    "  created_at timestamptz not null default now()",
    "",
    "  partition by tenant",
    "}",
  ].join("\n"),
  mark: "  partition by",
  outputs: [
    {
      icon: "database",
      name: "Postgres table",
      step: "tide apply",
      lang: "sql",
      code: [
        'ALTER TABLE "atlantis"."shop_order" FORCE ROW LEVEL SECURITY;',
        'CREATE POLICY "shop_order_tenant_isolation" ON "atlantis"."shop_order"',
        '  AS RESTRICTIVE USING ("tenant" = atlantis.current_partition())',
        '  WITH CHECK ("tenant" = atlantis.current_partition());',
      ],
      mark: '"tenant" = atlantis.current_partition()',
      text: "Row-level security limits each request to its own tenant's rows.",
    },
    {
      icon: "plugs",
      name: "API",
      step: "tide apply",
      lang: "",
      rpcs: ["GetOrder", "BatchGetOrder", "QueryOrder", "CreateOrder", "UpdateOrder", "DeleteOrder"],
      code: [],
      mark: "",
      text: "Live as soon as the change applies. There is no server to deploy.",
    },
    {
      icon: "braces",
      name: "Typed client",
      step: "tide generate",
      lang: "python",
      code: [
        'orders = OrderClient(channel, tenant="acme")',
        "res = orders.get(order_pb2.GetOrderRequest(id=42))",
        "for order in orders.query_all(order_pb2.QueryOrderRequest()):",
        "    print(order.id, order.total)",
      ],
      mark: 'tenant="acme"',
      text: "Written into your repository, with a type for every entity, request and response.",
    },
  ],
};

export const closing = {
  heading: "Connect your first repository",
  text: "Install `tide`, run `tide init`, and plan your first change",
};

// The form beside it. It posts to Cloud's /api/enquiries, which stores the
// message in cloud.enquiries.
export const enquiry = {
  heading: "Get in touch",
  text: "Ask a question or tell us what you are building",
  email: "Work email",
  message: "Message",
  send: "Send",
  sending: "Sending…",
  sent: "Thanks. Your message was sent.",
  limited: "Too many messages from your network. Try again in a minute.",
  failed: "Your message did not send. Try again in a moment.",
};
