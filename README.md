# Sensitive Data Management (SDM)

SDM is a Go toolset for separating sensitive data (PII) from append-only chain
data using Protobuf annotations. From a single annotated `.proto` file it
generates:

- Go GORM structs (`{Name}Pii`, `{Name}Chain`, `{Name}View`, optionally `{Name}PiiAudit`) plus a `View.AsBaseModel()` converter
- PostgreSQL DDL (PII table, chain table, combined view, version trigger; optionally an audit table + AFTER UPDATE/DELETE trigger; optionally a state-machine trigger + partial unique index for chain drafts)
- A type-safe repository whose surface depends on which optional features are enabled (see [Repository surface](#repository-surface-per-message))
- A single ctx-based actor channel (`WithActor`) that populates `created_by` on PII / chain and `changed_by` on audit rows

Two config knobs gate optional behavior:

- `create-audit-tables` (default `true`) — emit per-PII audit tables, the AFTER UPDATE/DELETE trigger, the `{Name}PiiAudit` struct, and `Repo.AuditLog`. See [PII audit log](#pii-audit-log).
- `chain-drafts` (default `false`) — opt-in draft/commit workflow on chain rows. Each chain row carries a status (DRAFTED / CREATED / DROPPED); the repo emits `DraftChain` / `CommitChain` / `DropChain` plus `Upsert` / `Update` (and SaveAll / SaveChain are NOT emitted); Fetch takes a `drafted bool` parameter. See [Chain drafts](#chain-drafts-opt-in).

A runnable end-to-end demo lives at
[sdm-tool/sdm-example/demo](https://github.com/jinuthankachan/sdm-examples).

## Features

### Field annotations (`sdmprotos/annotations.proto`)

| Annotation | Effect |
|---|---|
| `(sdm.primary_key) = true` | Column is the PII table primary key. |
| `(sdm.auto_increment) = true` | Generates `BIGSERIAL` in SQL and `autoIncrement` GORM tag; assigned value is copied back to the model on Save. |
| `(sdm.chain_identifier_key) = true` | Field's value is used as the chain table key (defaults to the PK if absent). Lets you use an opaque `user_id` string while the PK stays a numeric `id`. |
| `(sdm.pii) = true` | Column lives in `pii_{name}s` (sensitive, single row per record). |
| `(sdm.query_index) = true` | Column lives in PII for indexed lookups (no `pii` flag needed). |
| `(sdm.hashed) = true` | Adds a `hashed_{field}` chain row containing `sha256(value)`. Combines freely with `pii`. |
| `(sdm.unique) = true` | Emits a SQL `UNIQUE` constraint **and** generates `FetchBy{Field}` / `ExistsBy{Field}` methods. |
| `(sdm.references) = "Type.field"` | Emits a foreign key. The referenced field must be `UNIQUE` or `PRIMARY KEY`. Reference fields are placed in the PII table. |
| `(sdm.json) = true` | String field stored as Postgres `JSONB`; Go side uses `datatypes.JSON`. |

### Type handling

| Proto type | PII Go type | Chain serialization | View Go type |
|---|---|---|---|
| `string`, `int32`, `int64`, `bool` | Native | `fmt.Sprintf("%v", …)` | Native |
| `enum` | Typed enum | `fmt.Sprintf("%v", …)` (enum's registered name) | `string` (recovered to typed enum by `AsBaseModel` via `EnumType_value`) |
| `string` + `(sdm.json) = true` | `datatypes.JSON` | Raw JSON text | `datatypes.JSON` |
| `google.protobuf.Timestamp` | `time.Time` (via `.AsTime()`) | `time.RFC3339Nano` text (view casts back via `::timestamptz`) | `time.Time` |
| Nested `MessageType` | `*MessageType` (with `serializer:protojson`) | `protojson.Marshal(...)` | `*MessageType` (auto-decoded by serializer) |
| `repeated string` | `pq.StringArray` (`text[]`) | `pgArrayLiteral` → `{a,b,c}` | `pq.StringArray` (`text[]`) |
| `repeated MessageType` | `[]*MessageType` (with `serializer:protojsonArray`, stored `jsonb`) | JSON array, element-wise protojson | `[]*MessageType` (auto-decoded by serializer) |

Postgres `timestamptz` has microsecond precision (6 fractional digits) — `time.Time`
values with nanosecond precision get truncated on round-trip.

The `protojsonArray` serializer (auto-emitted in `sdm_helpers.go` when any
recorded message has a `repeated MessageType` field) handles
`[]*MessageType ↔ JSON array bytes` for both the PII column and the View
column. Empty / nil slices round-trip as the literal `[]`.

### Baked-in audit + soft-delete

Every PII table receives four columns by default:

```sql
created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
deleted_at TIMESTAMP WITH TIME ZONE NULL,
created_by TEXT NOT NULL DEFAULT '',
```

`created_by` is set at INSERT from the actor on the ctx (see [Actor
attribution](#actor-attribution) below) and is preserved across upserts.
There is **no** `updated_by` column — "who last updated this row" lives
per-change in `audit_pii_{name}s.changed_by` (see [PII audit log](#pii-audit-log)).
Dropping the column avoids drift between row-level state and the audit
trail and removes a redundant write on every upsert.

The generated PII and View structs carry the timestamps as `time.Time` /
`gorm.DeletedAt`. `db.Delete(&xPii)` performs soft-delete (sets
`deleted_at = NOW()`); all generated `Fetch` / `FetchBy{X}` / `Exists` /
`ExistsBy{X}` methods append an explicit `WHERE deleted_at IS NULL` filter
so soft-deleted rows stay hidden.

### Actor attribution

Pass the actor identifier on ctx with `{pkg}.WithActor(ctx, actorID)`
once at a request boundary (HTTP/gRPC middleware, batch job startup, CLI
entrypoint). All downstream `Save` / `SaveAll` / `SaveChain` calls
inherit it:

```go
ctx := user.WithActor(ctx, "alice@example.com")
repo.SaveAll(ctx, u, true)
// → pii.created_by   = "alice@example.com"   (at INSERT only)
// → chain.created_by = "alice@example.com"   (every appended chain row)
// → audit.changed_by = "alice@example.com"   (every UPDATE / DELETE)
```

The actor lands in three sinks:

| Sink | When written | Notes |
|---|---|---|
| `pii_{name}s.created_by` | INSERT only | Immutable across upserts — first writer wins. |
| `chain_{name}s.created_by` | Every new chain row | Append-only — each version records who appended it. |
| `audit_pii_{name}s.changed_by` | Every UPDATE / DELETE | Written by the AFTER trigger from the `sdm.actor` Postgres session variable. |

Repos that aren't wrapped with `WithActor` (bare `context.Background()`)
record `""` for all three. Direct GORM calls that bypass the generated
Save methods (`db.Exec("UPDATE …")`, `db.Unscoped().Delete(...)`) still
fire the audit trigger but record `""` because they don't set the
session variable.

### Chain versioning

The chain table's `(key, field_name, version)` is a composite primary key.
A `BEFORE INSERT` trigger sets `version = MAX(version) + 1` scoped to
`(key, field_name)`, so each field's history is a per-(record, field)
sequence — globally `1, 2, 3 …` per field, not a single sequence across the
whole table. Chain history is append-only and never rewritten.

**Skip-if-unchanged.** Every chain-writing method (`SaveAll(_, true)` in OFF
mode; `DraftChain` / `Save` / `Upsert` / `Update` in ON mode) first reads
the latest stored value per chain field for the key (one `SELECT DISTINCT
ON (field_name)`), then appends a new version only when the byte-form
differs. A re-save with identical data produces zero new chain rows;
partial changes bump only the affected fields. Chain is a history of
*changes*, not of saves. In chain-drafts mode the baseline is the latest
CREATED row only, so re-drafting a value identical to the last committed
state is a no-op even if intervening drafts were dropped.

Chain row timestamps (`created_at`) are stored as `TIMESTAMP WITH TIME ZONE`
so version history surfaces with the correct offset regardless of host /
server tz drift. Each chain row also carries a `created_by TEXT` column
populated from the same actor that wrote it (see [Actor
attribution](#actor-attribution)).

### Chain drafts (opt-in)

Enabled by setting `chain-drafts: true` in `sdm.cfg.yaml` (or
`--sdm_opt=chain-drafts=true` for direct `protoc-gen-sdm` usage). Stages
chain changes as drafts that callers can later commit or drop, with the
state machine enforced by a Postgres trigger.

**Schema additions** (ON mode):

```sql
-- New column on every chain_<name>s table
status TEXT NOT NULL DEFAULT 'CREATED'
  CHECK (status IN ('DRAFTED', 'CREATED', 'DROPPED'))

-- At-most-one DRAFTED row per (key, field_name) — DB-enforced
CREATE UNIQUE INDEX chain_<name>s_one_draft
  ON chain_<name>s (key, field_name)
  WHERE status = 'DRAFTED';

-- BEFORE UPDATE trigger that allows DRAFTED→CREATED, DRAFTED→DROPPED, or
-- "status unchanged"; everything else raises an exception.
```

**Two views** are emitted instead of one:

- `<name>s` — committed view (filters chain JOINs to `status='CREATED'`). What `Fetch(_, _, drafted=false)` reads.
- `<name>s_with_drafts` — overlay view (filters chain JOINs to `status<>'DROPPED'`, so a DRAFTED value supersedes the prior CREATED). What `Fetch(_, _, drafted=true)` reads.

Both views additionally expose `has_pending_drafts bool` — an `EXISTS` subquery against `chain_<name>s` for any DRAFTED row at this key. Surfaced on the View struct as `HasPendingDrafts`, independent of which view you queried. Use it as a signal that "this record has uncommitted changes" without paying a second round-trip.

**Repository surface** changes when ON:

| Replaces | New |
|---|---|
| `SaveAll(ctx, m, true)` | `Upsert(ctx, m)` + `CommitChain(ctx, key, txHash)` |
| `SaveAll(ctx, m, false)` | `Upsert(ctx, m)` (drafts the chain side; commit later or drop) |
| `SaveChain(ctx, m)` | `DraftChain(ctx, m)` + `CommitChain(ctx, key, txHash)` |
| — | `Update(ctx, m)` — strict UPDATE (errors with `gorm.ErrRecordNotFound` if missing) + `DraftChain` |
| — | `DropChain(ctx, key)` — promote DRAFTED → DROPPED for that key |
| `Fetch(ctx, pk)` | `Fetch(ctx, pk, drafted bool)` — `false` reads `<name>s`; `true` reads `<name>s_with_drafts` |

`Save` (PII strict INSERT) is emitted in both modes; in ON mode it also chains into `DraftChain` after the PII INSERT, all in the same transaction. `Exists` / `ExistsBy*` / `ChangeLog` / `AuditLog` are unchanged.

**Workflow**:

```go
ctx := invoice.WithActor(ctx, "alice@example.com")

// 1. Save: PII committed; chain rows staged as DRAFTED.
_ = repo.Save(ctx, &invoice.Invoice{
    InvoiceId: "inv_1", SellerId: "u_1", BuyerId: "u_2",
    Amount: 10000, Tags: []string{"draft"},
})

// 2. Committed view doesn't show the chain values yet…
v, _ := repo.Fetch(ctx, "inv_1", false)
fmt.Println(v.Amount, v.HasPendingDrafts) // 0 true

// 3. …but the overlay does.
v, _ = repo.Fetch(ctx, "inv_1", true)
fmt.Println(v.Amount, v.Tags)             // 10000 [draft]

// 4. Decide: commit (with optional tx_hash) or drop.
_ = repo.CommitChain(ctx, "inv_1", "tx-abc-123")
//   …or:
// _ = repo.DropChain(ctx, "inv_1")

// 5. Committed view now reflects the change; HasPendingDrafts flips false.
v, _ = repo.Fetch(ctx, "inv_1", false)
fmt.Println(v.Amount, v.TxHash, v.HasPendingDrafts) // 10000 tx-abc-123 false
```

**Sentinel error**. `DraftChain` (and by extension `Save` / `Upsert` /
`Update` when chain-drafts is ON) returns `ErrPendingDraftExists` if any
chain field of the record already has a pending DRAFTED row. The caller's
recourse is to commit (`CommitChain`) or drop (`DropChain`) the existing
draft first.

```go
if err := repo.Upsert(ctx, inv); err != nil {
    if errors.Is(err, invoice.ErrPendingDraftExists) {
        // surface to caller — they need to resolve the pending draft
    }
    return err
}
```

**Known caveat — half-state visibility.** Because `Save` / `Upsert` /
`Update` commit the PII row immediately but stage chain rows as DRAFTED,
the committed view will show the PII columns updated and the chain
columns NULL (or stale) until `CommitChain` runs. `HasPendingDrafts`
flags this on every read; pass `drafted=true` to read the overlay. Atomic
"PII + chain commit" is on the roadmap — for now the explicit two-step
flow is the contract.

**Testing pattern**. Because the repo surface differs across modes,
existing demo tests that use `SaveAll` are tagged `//go:build !chaindrafts`
and chain-drafts tests are tagged `//go:build chaindrafts`. Run against an
ON-mode generation with `go test -tags chaindrafts ./integration/...`.

### Change log

The generated `Repo.ChangeLog(ctx, key)` returns the full per-field version
history for one record:

```go
type ChangeLogEntry struct {
    Value     string    // raw chain field_value (TEXT)
    Timestamp time.Time // chain row created_at
}
type ChangeLog map[string]map[int64]ChangeLogEntry // field_name → version → entry
```

Soft-deleted PII rows do **not** mask chain history — chain entries persist
independently. Returns `gorm.ErrRecordNotFound` when the key has no chain rows.

### PII audit log

When enabled (default), every PII table gets a sibling `audit_pii_{name}s`
table plus an `AFTER UPDATE OR DELETE` trigger that captures the row as it
existed BEFORE each change. `INSERT` is not audited — the chain table
already records the newly-introduced values.

Schema:

```sql
CREATE TABLE audit_pii_users (
  id BIGSERIAL PRIMARY KEY,
  ref_id TEXT NOT NULL,                          -- PK as text (composite PKs joined with ':')
  last_value JSONB NOT NULL,                     -- row_to_json(OLD)
  change_type TEXT NOT NULL,                     -- 'UPDATE' or 'DELETE' (TG_OP)
  changed_by TEXT NOT NULL DEFAULT '',           -- from session var sdm.actor
  changed_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP
);
```

**Attribution** flows through the ctx-based actor described above. The
generated `Save` / `SaveAll` methods run `SELECT set_config('sdm.actor', $1, true)`
inside the same transaction as the PII write; the trigger reads it via
`current_setting('sdm.actor', true)`. Because `SET LOCAL` (via
`set_config(..., true)`) is transaction-scoped, the attribution does not
leak between requests.

**Reading the history** — `repo.AuditLog(ctx, pk)` returns
`[]{Name}PiiAudit` in chronological order:

```go
rows, _ := userRepo.AuditLog(ctx, u.Id)
for _, r := range rows {
    fmt.Printf("%s by %s at %s: %s\n",
        r.ChangeType, r.ChangedBy, r.ChangedAt, string(r.LastValue))
}
```

GORM soft-deletes (`db.Delete(&pii)` when the struct has
`gorm.DeletedAt`) appear as `change_type = 'UPDATE'` because the underlying
SQL is `UPDATE … SET deleted_at = NOW()`. Hard deletes
(`db.Unscoped().Delete(...)`) appear as `'DELETE'`.

**Disabling audit tables** — set `create-audit-tables: false` in
`sdm.cfg.yaml` (or pass `--sdm_opt=create-audit-tables=false` when
invoking `protoc-gen-sdm` directly from buf). When off, the generator
skips:

- the `audit_pii_{name}s` table and its trigger function
- the `{Name}PiiAudit` Go struct
- the `Repo.AuditLog` method
- the `SELECT set_config('sdm.actor', …)` call inside Save/SaveAll (no
  trigger to read it)

The actor still populates `pii.created_by` and `chain.created_by`; only
the per-change history disappears. If your test suite includes audit
assertions, tag them with `//go:build !noaudit` so they're skipped by
`go test -tags noaudit` against an audit-off generation. The demo
integration suite uses this pattern — see
[sdm-example/demo/integration/audit_test.go](https://github.com/jinuthankachan/sdm-examples/blob/main/demo/integration/audit_test.go).

### View → base model

Every `{Name}View` carries an `AsBaseModel()` method that returns a fresh
`*{Name}` proto. Use it when you've fetched the view and want to mutate +
re-`SaveAll` without manually mapping fields. The converter handles:

- scalar fields — direct assignment
- `enum` fields — `string` → typed enum via the proto-generated `{EnumType}_value` map lookup
- `*Message` fields — direct assignment (`serializer:protojson` already decoded on read)
- `google.protobuf.Timestamp` — `time.Time` → `timestamppb.New(...)` (skipped if zero)
- `(sdm.json)=true` strings — `datatypes.JSON` → `string`
- repeated scalar — `pq.StringArray` → `[]string`
- repeated `*Message` — direct assignment (`[]*Message` already decoded by the `protojsonArray` serializer)

Audit columns (`CreatedAt` / `UpdatedAt` / `DeletedAt` / `CreatedBy` / `TxHash`)
and `hashed_*` sidecar columns have no counterpart on the base proto and
are dropped.

## Installation

```bash
go install github.com/kapow-tech/sdm/cmd/sdm@latest
```

Two more steps put the project on a clean footing:

```bash
sdm config   # writes sdm.cfg.yaml in the current directory
sdm setup    # installs protoc-gen-go, buf, protoc-gen-sdm; exports SDM protos
```

## Configuration (`sdm.cfg.yaml`)

```yaml
# Version of the sdm to use
sdm: "dev"

# Where the SDM annotation protos were exported by `sdm setup` (relative to this file)
sdm-proto: "proto/"

# Protos to compile and generate from (relative to this file)
user-protos:
  - "proto/user/user.proto"
  - "proto/invoice/invoice.proto"

# Where to write generated Go files
output: "models/"

# Where to write generated SQL files (defaults to `output` when omitted)
output-sql: "models/sql/"

# Emit audit_pii_{name}s tables + AFTER UPDATE/DELETE trigger +
# {Name}PiiAudit struct + Repo.AuditLog method. Defaults to true.
# When false, the actor still flows into pii.created_by /
# chain.created_by — those columns are independent.
create-audit-tables: true

# Opt-in chain draft/commit workflow. When true the generator swaps
# SaveAll for Upsert/Update + DraftChain/CommitChain/DropChain, emits
# a status column + partial unique index + state-machine trigger on
# every chain table, emits two views (committed + with-drafts), and
# Fetch / FetchBy* gain a trailing `drafted bool` parameter. Defaults
# to false. See the "Chain drafts (opt-in)" section below.
chain-drafts: false
```

All paths are resolved relative to the directory containing `sdm.cfg.yaml`.
Both knobs are also exposed to direct buf/protoc usage:
`--sdm_opt=create-audit-tables=false`, `--sdm_opt=chain-drafts=true`.

## Usage

A complete runnable example is at
[sdm-example/demo](https://github.com/jinuthankachan/sdm-examples).

### 1. Annotate your `.proto`

```proto
syntax = "proto3";
package invoice;

import "proto/sdmprotos/annotations.proto";

option go_package = "demo/models/invoice";

message Invoice {
  string invoice_id = 1 [(sdm.primary_key) = true, (sdm.chain_identifier_key) = true];
  string seller_gst = 2 [(sdm.pii) = true, (sdm.hashed) = true];
  string buyer_gst  = 3 [(sdm.pii) = true, (sdm.hashed) = true];
  string seller_id  = 4 [(sdm.references) = "User.user_id"];
  string buyer_id   = 5 [(sdm.references) = "User.user_id"];
  int64  amount     = 6;
  string metadata   = 7 [(sdm.json) = true];
  Money  price      = 8 [(sdm.pii) = true];
  repeated string tags  = 9;
  repeated Money  items = 10;
}

message Money {
  int64  value = 1;
  string unit  = 2;
}
```

`User` lives in a sibling `user.proto`:

```proto
message User {
  int64  id      = 1 [(sdm.primary_key) = true, (sdm.auto_increment) = true];
  string user_id = 2 [(sdm.pii) = true, (sdm.chain_identifier_key) = true, (sdm.unique) = true];
  string email   = 3 [(sdm.pii) = true, (sdm.hashed) = true, (sdm.unique) = true];
  string name    = 5 [(sdm.pii) = true];
  string pan     = 6 [(sdm.unique) = true];
  string country = 7;
}
```

### 2. Generate

A minimal `sdm.cfg.yaml` (see [Configuration](#configuration-sdmcfgyaml)
for the full reference, including `create-audit-tables`):

```yaml
sdm: "dev"
sdm-proto: "proto/"
user-protos:
  - "proto/user/user.proto"
  - "proto/invoice/invoice.proto"
output: "models/"
output-sql: "models/sql/"
```

```bash
sdm generate
```

Per proto, four files are emitted:
- `{name}.pb.go` — standard protobuf code
- `{name}_sdm_model.go` — `{Name}Pii`, `{Name}Chain`, `{Name}View` structs (plus `{Name}PiiAudit` when `create-audit-tables: true`)
- `{name}_sdm_schema.sql` — `CREATE TABLE`s, the version trigger, the view (plus the audit table + trigger when enabled)
- `{name}_sdm_repo.go` — GORM repository

A single `sdm_helpers.go` per package holds `pgArrayLiteral` (for repeated
scalar fields), the `ChangeLog` / `ChangeLogEntry` types, the
`WithActor` / `actorFromContext` ctx helpers, `ErrPendingDraftExists` (when
`chain-drafts: true`), and — when nested messages are present — the
`protojson` / `protojsonArray` GORM serializers.

### 3. Use in Go

The snippet below assumes the OFF-mode API (`chain-drafts: false`, the
default). For the draft/commit workflow, see [Chain drafts](#chain-drafts-opt-in).

```go
import (
    "context"
    "gorm.io/driver/postgres"
    "gorm.io/gorm"
    "demo/models/invoice"
    "demo/models/user"
)

db, _ := gorm.Open(postgres.Open(dsn), &gorm.Config{})

// Attribute every downstream write to a single actor. Set once at a
// request / job boundary; all repo calls on this ctx inherit it.
ctx := user.WithActor(context.Background(), "alice@example.com")

userRepo := user.NewUserRepo(db)
// SaveAll(_, true) upserts PII + appends a chain version per changed field.
// pii.created_by + chain.created_by are populated from the ctx actor.
_ = userRepo.SaveAll(ctx, &user.User{
    UserId: "u_001", Email: "alice@example.com", Name: "Alice", Pan: "ABCDE1234F", Country: "IN",
}, true)

repo := invoice.NewInvoiceRepo(db)
inv := &invoice.Invoice{
    InvoiceId: "inv_001",
    SellerGst: "27AAA…", BuyerGst: "29BBB…",
    SellerId:  "u_001", BuyerId: "u_002",
    Amount:    10000,
    Metadata:  `{"source":"api"}`,
    Price:     &invoice.Money{Value: 10000, Unit: "INR"},
    Tags:      []string{"urgent", "paid"},
    Items:     []*invoice.Money{{Value: 9000, Unit: "INR"}, {Value: 1000, Unit: "INR"}},
}
_ = repo.SaveAll(ctx, inv, true)

// Save is a strict INSERT on the PII row — errors on PK / unique conflict.
// Use it when you want the conflict to surface as an error rather than an upsert.
err := repo.Save(ctx, &invoice.Invoice{InvoiceId: "inv_001", /* … */})
// err is a Postgres unique-violation since inv_001 already exists.
_ = err

view, _ := repo.Fetch(ctx, "inv_001")
// view.Price is *Money, view.Items is datatypes.JSON, view.Tags is pq.StringArray.
// view.CreatedAt / view.UpdatedAt / view.DeletedAt / view.CreatedBy are populated automatically.
// (For "who last updated this row", call repo.AuditLog and read the latest row's ChangedBy.)

// AsBaseModel: convert the view back to the base proto for re-saves.
roundTrip := view.AsBaseModel()
roundTrip.Amount = 12000
_ = repo.SaveAll(ctx, roundTrip, true) // chain v2 for amount; other fields unchanged → no-op

// SaveAll(_, false): upsert PII only, leave chain alone.
_ = repo.SaveAll(ctx, roundTrip, false)

// SaveChain: append chain entries only (still skip-if-unchanged).
_ = repo.SaveChain(ctx, roundTrip)

// Full per-field version history.
log, _ := repo.ChangeLog(ctx, "inv_001")
// log["amount"][1].Value == "10000"
// log["amount"][2].Value == "12000"
// log["amount"][2].Timestamp is the chain row's timestamptz created_at

// Soft-delete via GORM:
_ = db.Delete(&invoice.InvoicePii{InvoiceId: "inv_001"}).Error
// Subsequent Fetch / Exists return ErrRecordNotFound / false.
// ChangeLog still returns the chain history — soft-delete does not mask it.

// Per-change audit history (audit-on only).
audit, _ := repo.AuditLog(ctx, "inv_001")
for _, r := range audit {
    fmt.Printf("%s by %q at %s\n", r.ChangeType, r.ChangedBy, r.ChangedAt)
}
```

## CLI Reference

| Command | Description |
|---|---|
| `sdm setup` | Installs `protoc-gen-go`, `buf`, `protoc-gen-sdm`; exports SDM annotation protos to a local directory. |
| `sdm config` | Writes a default `sdm.cfg.yaml`. |
| `sdm generate` | Compiles user protos and writes the four generated files per message. Flags: `--proto`, `--out`, `--cfg` (default `sdm.cfg.yaml`). |

## Using with buf directly

```bash
go install github.com/kapow-tech/sdm/cmd/protoc-gen-sdm@latest
```

```yaml
# buf.gen.yaml
version: v1
plugins:
  - plugin: go
    out: .
    opt: paths=source_relative
  - plugin: sdm
    out: .
    opt: paths=source_relative
```

```bash
buf generate
```

## Generated schema layout

- **`pii_{name}s`** — primary key, PII / query-index / FK columns, plus the
  three timestamp audit columns (`created_at`, `updated_at`, `deleted_at`,
  all `TIMESTAMP WITH TIME ZONE`) and `created_by TEXT`. Soft-deleted rows
  have non-NULL `deleted_at`. `created_by` is set at INSERT from the ctx
  actor and preserved across upserts.
- **`audit_pii_{name}s`** *(emitted when `create-audit-tables: true`)* —
  `(id, ref_id, last_value, change_type, changed_by, changed_at)`.
  Populated by the `audit_pii_{name}s_log_trigger` `AFTER UPDATE OR DELETE`
  trigger on `pii_{name}s`. `last_value` is `row_to_json(OLD)::jsonb`;
  `change_type` is the trigger's `TG_OP`; `changed_by` reads from the
  `sdm.actor` Postgres session variable (transaction-scoped via `SET LOCAL`).
- **`chain_{name}s`** — `(key, field_name, version, tx_hash, field_value, created_at, created_by)`.
  `created_at` is `TIMESTAMP WITH TIME ZONE`; `created_by TEXT` records the
  actor that appended the row. The `version` is auto-assigned per
  `(key, field_name)` by the `chain_{name}s_set_version_trigger`
  `BEFORE INSERT` trigger; field values are TEXT, with the view casting back
  to `::jsonb`, `::timestamptz`, or `text[]` where appropriate.
  *When chain-drafts is enabled*: an additional `status TEXT NOT NULL
  DEFAULT 'CREATED' CHECK (status IN ('DRAFTED', 'CREATED', 'DROPPED'))`
  column, a partial unique index `chain_{name}s_one_draft (key, field_name)
  WHERE status='DRAFTED'`, and a `BEFORE UPDATE` trigger
  `chain_{name}s_status_guard_trigger` enforcing legal transitions
  (DRAFTED → CREATED, DRAFTED → DROPPED, status unchanged).
- **`{name}s` (view)** — joins `pii_{name}s p` with one `LEFT JOIN`
  per chain-stored field (`DISTINCT ON (key, field_name) … ORDER BY
  version DESC` to pick the latest version). PII audit columns including
  `created_by` are surfaced; per-update history lives in
  `audit_pii_{name}s` (when enabled) rather than on the row.
  *When chain-drafts is enabled*: each chain-side JOIN subquery filters
  to `status='CREATED'` (committed only); a sibling view `{name}s_with_drafts`
  filters to `status<>'DROPPED'` instead (overlay). Both views expose
  `has_pending_drafts bool` via an `EXISTS` subquery, surfaced on the
  View struct as `HasPendingDrafts`.

## Repository surface (per message)

The exact method set depends on the `chain-drafts` knob — OFF mode emits
the familiar `Save` / `SaveAll` pair; ON mode emits the draft-workflow
trio (`Upsert` / `Update` / `DraftChain` / `CommitChain` / `DropChain`)
instead. Read-side methods (`Fetch`, `Exists`, `ChangeLog`, `AuditLog`)
exist in both modes; `Fetch` gains a trailing `drafted bool` parameter in
ON mode.

### Common to both modes

| Method | Notes |
|---|---|
| `Save(ctx, *T)` | **Strict INSERT** of the PII row. Returns the driver-native error on PK / unique conflict. In OFF mode does not touch the chain table; in ON mode also calls `DraftChain` in the same transaction (chain rows staged as DRAFTED). Honors `WithActor` (writes `pii.created_by`). |
| `Exists(ctx, pk)` / `ExistsBy{Unique}` | Counts on the PII table with the `deleted_at IS NULL` filter. Not draft-aware — existence is answered by committed state. |
| `ChangeLog(ctx, key)` | Returns the full per-field version history as `map[field_name]map[version]{Value, Timestamp}`. Returns `gorm.ErrRecordNotFound` if no chain rows exist. |
| `AuditLog(ctx, pk)` *(audit-on only)* | Returns `[]{Name}PiiAudit` rows for one PII record, oldest first. Each row carries `LastValue` (OLD as JSONB), `ChangeType` (`'UPDATE'`/`'DELETE'`), `ChangedBy` (from the `WithActor` ctx), and `ChangedAt`. Not emitted when `create-audit-tables: false`. |

### OFF mode (`chain-drafts: false`, default)

| Method | Notes |
|---|---|
| `SaveAll(ctx, *T, withChain bool)` | Upserts the PII row (`ON CONFLICT … DO UPDATE` on the chain identifier key); when `withChain=true`, also appends new chain versions for every field whose value changed (skip-if-unchanged). Honors `WithActor`. |
| `Fetch(ctx, pk)` | Reads `*TView` from the view, filtered by `deleted_at IS NULL`. |
| `FetchBy{Unique}(ctx, val)` | Generated for every `(sdm.unique)` field. |

### ON mode (`chain-drafts: true`)

| Method | Notes |
|---|---|
| `Upsert(ctx, *T)` | PII upsert + `DraftChain` (chain rows staged as DRAFTED). Honors `WithActor`. |
| `Update(ctx, *T)` | Strict PII UPDATE — returns `gorm.ErrRecordNotFound` when the row doesn't exist (no insert). Followed by `DraftChain`. Honors `WithActor`. |
| `DraftChain(ctx, *T)` | Standalone draft entry — appends DRAFTED chain rows for fields differing from the latest CREATED. Returns `ErrPendingDraftExists` if a draft is already pending for any field of this record (resolve via `CommitChain` or `DropChain` first). Honors `WithActor` (writes `chain.created_by`). |
| `CommitChain(ctx, key…, txHash string)` | Promotes every DRAFTED row for this key to CREATED in a single UPDATE, stamping `txHash` on the promoted rows (pass `""` if not applicable). Idempotent — no-op when no drafts exist. Trigger-enforced transition. |
| `DropChain(ctx, key…)` | Promotes every DRAFTED row for this key to DROPPED in a single UPDATE. Idempotent. |
| `Fetch(ctx, pk, drafted bool)` | `drafted=false` reads the committed view `<name>s`; `drafted=true` reads the overlay view `<name>s_with_drafts`. Both filtered by `deleted_at IS NULL`. View struct's `HasPendingDrafts` is populated regardless of which is queried. |
| `FetchBy{Unique}(ctx, val, drafted bool)` | Same `drafted` semantics. |

`SaveAll` and `SaveChain` are **not emitted** in ON mode — their atomic
"PII + chain commit" semantics live in `Upsert` followed by an explicit
`CommitChain`.

### View methods

| Method | Notes |
|---|---|
| `View.AsBaseModel() *T` | Converts the view row back to the base proto model (Timestamp → `timestamppb`, repeated message JSON → `[]*Message`, datatypes.JSON → string, pq.StringArray → []string). Audit columns and hashed sidecars are dropped. |
