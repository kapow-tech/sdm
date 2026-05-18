// Package sdm is the shared runtime used by every SDM-generated repository.
// Routing the actor-context helpers through one package gives all repos a
// single context key — without this, each generated package would declare
// its own actorKey{} struct and a context set by one package's helper would
// be invisible to another package's repo.
package sdm

import "context"

// actorKey is the context key carrying the actor identifier that populates
// created_by on PII rows, created_by on chain rows, and changed_by on audit
// rows (via the AFTER UPDATE/DELETE trigger). Unexported so callers must go
// through CtxWithActor / ActorFromContext.
type actorKey struct{}

// CtxWithActor returns a derived context that propagates the actor identifier
// to any PII mutation made through a generated repo. Inside the same
// transaction as the write the repo sets the Postgres session variable
// `sdm.actor` from this value (scoping the attribution to that transaction)
// AND assigns it to the created_by column on INSERT. The AFTER UPDATE/DELETE
// trigger reads the same session variable to populate
// audit_pii_<name>s.changed_by. Direct GORM operations (db.Exec / db.Delete)
// that do not pass through a generated repo method record '' for changed_by.
func CtxWithActor(ctx context.Context, actorID string) context.Context {
	return context.WithValue(ctx, actorKey{}, actorID)
}

// ActorFromContext extracts the actor identifier installed by CtxWithActor,
// or "" if absent. Called by every generated repo method that mutates PII or
// chain rows; user code rarely needs it directly.
func ActorFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(actorKey{}).(string); ok {
		return v
	}
	return ""
}
