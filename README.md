# Sensitive Data Management (SDM)

SDM is a toolset for Golang projects to manage sensitive data (PII) by separating it from public chain data using Protobuf annotations. It automatically generates Go models, SQL schemas, and Repository functions to handle the data flow.

## Features

*   **Proto Annotations**: Define `primary_key`, `pii`, `hashed`, etc., directly in your `.proto` files.
*   **Auto-Generated Go Models**: Creates GORM-compatible structs for PII tables, Chain tables, and combined Views.
*   **Auto-Generated SQL**: Generates `CREATE TABLE` and `CREATE VIEW` statements for PostgreSQL.
*   **Auto-Generated Repositories**: Generates type-safe `Save` and `Fetch` methods that handle:
    *   Splitting data into PII and Chain tables.
    *   Hashing fields marked as `hashed`.
    *   Reconstructing objects from the DB View.
*   **Integrated Toolchain**: The `sdm` CLI manages dependencies, setup, and generation, acting as a wrapper around standard tools like `buf` and `protoc`.

## Installation

1.  **Install the Tool**:
    ```bash
    go install github.com/kapow-tech/sdm/cmd/sdm@latest
    ```

2. **Configuration**:

    Generate a configuration file to manage your project settings.

    ```bash
    sdm config
    ```
    This creates `sdm.cfg.yaml` where you can customize output directories and input proto files.

3.  **Setup the Environment**:
    Run `sdm setup` to install required dependencies (`protoc-gen-go`, `buf`, `protoc-gen-sdm`), initialize the `buf` module, and download SDM proto definitions.
    ```bash
    sdm setup
    ```

## Usage

An example can be found at [SDM examples repo](https://github.com/kapow-tech/sdm-examples)

### 1. Define your Data Model

Create a `.proto` file (e.g., `proto/invoice/invoice.proto`) and import `annotations/annotations.proto`. Annotate your fields:

```proto
syntax = "proto3";
package invoice;

import "annotations/annotations.proto";

option go_package = "github.com/kapow-tech/sdm/proto/invoice";

message Invoice {
  string id = 1 [(sdm.primary_key) = true, (sdm.chain_identifier_key) = true];
  int64 invoice_number = 2 [(sdm.pii) = true];
  string seller_gst = 3 [(sdm.pii) = true, (sdm.hashed) = true];
  // ...
}
```

### 2. Generate Code

Run the `sdm` tool to generate the artifacts.

```bash
sdm generate --proto proto/invoice/invoice.proto --out gen_out
```

Or, if you have configured `sdm.cfg.yaml` with your protos:
```bash
sdm generate
```

This will compile the protos using the `sdm` directory (setup by `sdm setup`) as an import path and generate:
*   `invoice.pb.go`: Standard Protobuf Go code.
*   `invoice_sdm_model.go`: SDM Structs (`...Pii`, `...Chain`, `...View`).
*   `invoice_sdm_schema.sql`: SQL DDL for PII, Chain tables and Views.
*   `invoice_sdm_repo.go`: GORM Repository implementation.

### 3. Use in Go

```go
import (
    "context"
    "gorm.io/gorm"
    "github.com/kapow-tech/sdm/proto/invoice"
    // Ensure the annotations package is available if needed, usually implicitly handled by generated code imports
)

func main() {
    db, _ := gorm.Open(...) 
    repo := invoice.NewInvoiceRepo(db)

    // Save (Splits and Hashes automatically)
    err := repo.Save(ctx, &invoice.Invoice{
        Id: "inv_123",
        InvoiceNumber: 1001,
        SellerGst: "GST001",
    })

    // Fetch (reconstructs from View)
    view, err := repo.Fetch(ctx, "inv_123")
}
```

## CLI Reference

*   `sdm setup`: Installs dependencies (`protoc-gen-go`, `buf`, `protoc-gen-sdm`), initializes `buf`, and exports SDM protos to a local `sdm/` directory.
*   `sdm config`: Generates a default `sdm.cfg.yaml` file.
*   `sdm generate`: Compiles and generates code.
    *   `--proto`: Input proto file (optional if defined in config).
    *   `--out`: Output directory (optional if defined in config).
    *   `--cfg`: Path to config file (default `sdm.cfg.yaml`).

## Using with Buf directly (Not tested enough)

If you prefer using `buf` directly without the `sdm` wrapper:

1.  **Install the plugin**:
    ```bash
    go install github.com/kapow-tech/sdm/cmd/protoc-gen-sdm@latest
    ```
2.  **Configure `buf.gen.yaml`**:
    ```yaml
    version: v1
    plugins:
      - plugin: go
        out: .
        opt: paths=source_relative
      - plugin: sdm
        out: .
        opt: paths=source_relative
    ```
3.  **Generate**:
    ```bash
    buf generate
    ```

## Generated Schema Structure

*   **`pii_<name>s`**: Stores `pii` fields and `primary_key`.
*   **`chain_<name>s`**: key-value store for non-pii and `hashed` fields (EAV pattern).
*   **`<name>s` (View)**: Joins the PII table with the latest values from the Chain table.
