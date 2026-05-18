// Package generator is the SDM code generator. Each call to GenerateFile
// produces three artifacts for a single proto file — <prefix>_sdm_model.go,
// <prefix>_sdm_schema.sql, and <prefix>_sdm_repo.go — and GenerateHelpers
// emits a once-per-package sdm_helpers.go.
//
// The implementation is split across files by output artifact:
//
//	generator.go  — entry points (Options, DefaultOptions, GenerateFile)
//	helpers.go    — sdm_helpers.go emission + protojson serializers
//	models.go     — <prefix>_sdm_model.go emission (Go structs)
//	sql.go        — <prefix>_sdm_schema.sql emission (DDL + views)
//	repo.go       — <prefix>_sdm_repo.go emission (GORM repository)
//	fields.go     — per-field / per-message introspection used by all of the above
package generator

import (
	"google.golang.org/protobuf/compiler/protogen"
)

// Options configures optional generator features.
//
// CreateAuditTables controls whether each PII-backed message gets an
// audit_pii_<name>s table, the AFTER UPDATE/DELETE trigger, the
// <Name>PiiAudit Go struct, and the Repo.AuditLog method. When false,
// the actor still flows into pii.created_by / chain.created_by (those
// columns are independent), but the `sdm.actor` session-variable assignment
// is skipped — nothing reads it.
//
// ChainDrafts toggles the draft/commit workflow on chain rows. When true:
//   - chain rows carry a status (DRAFTED / CREATED / DROPPED)
//   - a partial unique index allows at most one DRAFTED per (key, field_name)
//   - a BEFORE UPDATE trigger enforces the state machine
//   - two views are emitted (committed-only and with-drafts)
//   - the repo emits DraftChain / CommitChain / DropChain methods plus
//     Upsert / Update (and SaveAll / SaveChain are NOT emitted)
//   - Create (PII strict INSERT) chains into DraftChain after the PII write
//   - Fetch / FetchBy* signatures gain a trailing `drafted bool` parameter
//   - the View struct gains a HasPendingDrafts bool field
type Options struct {
	CreateAuditTables bool
	ChainDrafts       bool
}

// DefaultOptions returns the options used when callers don't supply any —
// audit tables are emitted by default; the chain draft workflow is opt-in.
func DefaultOptions() Options {
	return Options{CreateAuditTables: true, ChainDrafts: false}
}

// GenerateFile generates the SDM artifacts for a single proto file.
// After iterating all files, call GenerateHelpers(gen, anyFile) exactly once.
func GenerateFile(gen *protogen.Plugin, file *protogen.File, opts Options) {
	if len(file.Messages) == 0 {
		return
	}

	// generate Go models
	generateModels(gen, file, opts)
	// generate SQL schema
	generateSQL(gen, file, opts)
	// generate GORM repository
	generateRepo(gen, file, opts)
}
