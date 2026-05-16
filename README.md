# Sensitive Data Management (SDM)

SDM is a Go toolset for separating sensitive data (PII) from append-only chain
data using Protobuf annotations. From a single annotated `.proto` file it
generates:

- Go GORM structs (`{Name}Pii`, `{Name}Chain`, `{Name}View`) plus a `View.AsBaseModel()` converter
- PostgreSQL DDL (PII table, chain table, combined view, version trigger)
- A type-safe repository (`Save`, `SaveAll`, `SaveChain`, `Fetch`, `FetchBy{Unique}`, `Exists`, `ExistsBy{Unique}`, `ChangeLog`)

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
| `string` + `(sdm.json) = true` | `datatypes.JSON` | Raw JSON text | `datatypes.JSON` |
| `google.protobuf.Timestamp` | `time.Time` (via `.AsTime()`) | `time.RFC3339Nano` text (view casts back via `::timestamptz`) | `time.Time` |
| Nested `MessageType` | `*MessageType` (with `serializer:protojson`) | `protojson.Marshal(...)` | `*MessageType` (auto-decoded by serializer) |
| `repeated string` | (not allowed in PII) | `pgArrayLiteral` → `{a,b,c}` | `pq.StringArray` (`text[]`) |
| `repeated MessageType` | (not allowed in PII) | JSON array, element-wise protojson | `datatypes.JSON` (`jsonb`) |

Postgres `timestamptz` has microsecond precision (6 fractional digits) — `time.Time`
values with nanosecond precision get truncated on round-trip.

### Baked-in audit + soft-delete

Every PII table receives three columns by default:

```sql
created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT CURRENT_TIMESTAMP,
deleted_at TIMESTAMP WITH TIME ZONE NULL,
```

The generated PII and View structs carry these as `time.Time` / `gorm.DeletedAt`.
`db.Delete(&xPii)` performs soft-delete (sets `deleted_at = NOW()`); all
generated `Fetch` / `FetchBy{X}` / `Exists` / `ExistsBy{X}` methods append
an explicit `WHERE deleted_at IS NULL` filter so soft-deleted rows stay
hidden.

### Chain versioning

The chain table's `(key, field_name, version)` is a composite primary key.
A `BEFORE INSERT` trigger sets `version = MAX(version) + 1` scoped to
`(key, field_name)`, so each field's history is a per-(record, field)
sequence — globally `1, 2, 3 …` per field, not a single sequence across the
whole table. Chain history is append-only and never rewritten.

**Skip-if-unchanged.** `SaveAll(_, true)` and `SaveChain` first read the
latest stored value per chain field for the key (one `SELECT DISTINCT ON
(field_name)`), then append a new version only when the byte-form differs.
A re-save with identical data produces zero new chain rows; partial changes
bump only the affected fields. Chain is a history of *changes*, not of saves.

Chain row timestamps (`created_at`) are stored as `TIMESTAMP WITH TIME ZONE`
so version history surfaces with the correct offset regardless of host /
server tz drift.

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

### View → base model

Every `{Name}View` carries an `AsBaseModel()` method that returns a fresh
`*{Name}` proto. Use it when you've fetched the view and want to mutate +
re-`SaveAll` without manually mapping fields. The converter handles:

- scalar / `*Message` fields — direct assignment
- `google.protobuf.Timestamp` — `time.Time` → `timestamppb.New(...)` (skipped if zero)
- `(sdm.json)=true` strings — `datatypes.JSON` → `string`
- repeated scalar — `pq.StringArray` → `[]string`
- repeated `*Message` — JSON-array bytes split + per-element `protojson`; malformed
  elements are silently skipped (best-effort)

Audit columns (`CreatedAt` / `UpdatedAt` / `DeletedAt` / `TxHash`) and `hashed_*`
sidecar columns have no counterpart on the base proto and are dropped.

## Installation

```bash
go install github.com/kapow-tech/sdm/cmd/sdm@latest
```

Two more steps put the project on a clean footing:

```bash
sdm config   # writes sdm.cfg.yaml in the current directory
sdm setup    # installs protoc-gen-go, buf, protoc-gen-sdm; exports SDM protos
```

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

With `sdm.cfg.yaml`:
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
- `{name}_sdm_model.go` — `{Name}Pii`, `{Name}Chain`, `{Name}View` structs
- `{name}_sdm_schema.sql` — `CREATE TABLE`s, the version trigger, the view
- `{name}_sdm_repo.go` — GORM repository

A single `sdm_helpers.go` per package holds `pgArrayLiteral` (for repeated
scalar fields), the `ChangeLog` / `ChangeLogEntry` types, and — when nested
messages are present — the `protojson` GORM serializer.

### 3. Use in Go

```go
import (
    "context"
    "gorm.io/driver/postgres"
    "gorm.io/gorm"
    "demo/models/invoice"
    "demo/models/user"
)

db, _ := gorm.Open(postgres.Open(dsn), &gorm.Config{})

userRepo := user.NewUserRepo(db)
// SaveAll(_, true) upserts PII + appends a chain version per changed field.
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
// view.CreatedAt / view.UpdatedAt / view.DeletedAt are populated automatically.

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
  three audit columns (all `TIMESTAMP WITH TIME ZONE`). Soft-deleted rows
  have non-NULL `deleted_at`.
- **`chain_{name}s`** — `(key, field_name, version, tx_hash, field_value, created_at)`.
  `created_at` is `TIMESTAMP WITH TIME ZONE`. The `version` is auto-assigned
  per `(key, field_name)` by the `chain_{name}s_set_version_trigger`
  `BEFORE INSERT` trigger; field values are TEXT, with the view casting back
  to `::jsonb`, `::timestamptz`, or `text[]` where appropriate.
- **`{name}s` (view)** — joins `pii_{name}s p` with one `LEFT JOIN`
  per chain-stored field (`DISTINCT ON (key, field_name) … ORDER BY
  version DESC` to pick the latest version). Audit columns are surfaced
  from the PII table.

## Repository surface (per message)

| Method | Notes |
|---|---|
| `Save(ctx, *T)` | **Strict INSERT** of the PII row only. Returns the driver-native error on PK / unique conflict. Does not touch the chain table. |
| `SaveAll(ctx, *T, withChain bool)` | Upserts the PII row (`ON CONFLICT … DO UPDATE` on the chain identifier key); when `withChain=true`, also appends new chain versions for every field whose value changed (skip-if-unchanged). |
| `SaveChain(ctx, *T)` | Chain appends only — also skip-if-unchanged per field. |
| `Fetch(ctx, pk)` | Reads `*TView` from the view filtered by `deleted_at IS NULL`. |
| `FetchBy{Unique}` | Generated for every `(sdm.unique)` field. |
| `Exists(ctx, pk)` / `ExistsBy{Unique}` | Counts on the PII table with the same soft-delete filter. |
| `ChangeLog(ctx, key)` | Returns the full per-field version history as `map[field_name]map[version]{Value, Timestamp}`. Returns `gorm.ErrRecordNotFound` if no chain rows exist. |

### View methods

| Method | Notes |
|---|---|
| `View.AsBaseModel() *T` | Converts the view row back to the base proto model (Timestamp → `timestamppb`, repeated message JSON → `[]*Message`, datatypes.JSON → string, pq.StringArray → []string). Audit columns and hashed sidecars are dropped. |
