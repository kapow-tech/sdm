package generator

import (
	"strings"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	sdm "github.com/kapow-tech/sdm/sdmprotos" // Import the generated code for annotations
)

// SdmOptions is the in-memory shape of the (sdm.*) field annotations
// extracted from a proto field. Populated by getFieldOptions.
type SdmOptions struct {
	PrimaryKey         bool
	ChainIdentifierKey bool // if set, this field is used as the chain key instead of the PK
	Pii                bool
	QueryIndex         bool
	Hashed             bool
	Unique             bool
	AutoIncrement      bool
	Json               bool // stored as Postgres JSONB and Go datatypes.JSON
	References         string
}

// getFieldOptions extracts the sdm annotations from a proto field. Boolean
// extensions default to false when absent; the References string defaults
// to "" when absent.
func getFieldOptions(field *protogen.Field) SdmOptions {
	opts := field.Desc.Options()

	getBool := func(ext protoreflect.ExtensionType) bool {
		if proto.HasExtension(opts, ext) {
			return proto.GetExtension(opts, ext).(bool)
		}
		return false
	}

	getString := func(ext protoreflect.ExtensionType) string {
		if proto.HasExtension(opts, ext) {
			return proto.GetExtension(opts, ext).(string)
		}
		return ""
	}

	return SdmOptions{
		PrimaryKey:         getBool(sdm.E_PrimaryKey),
		ChainIdentifierKey: getBool(sdm.E_ChainIdentifierKey),
		Pii:                getBool(sdm.E_Pii),
		QueryIndex:         getBool(sdm.E_QueryIndex),
		Hashed:             getBool(sdm.E_Hashed),
		Unique:             getBool(sdm.E_Unique),
		AutoIncrement:      getBool(sdm.E_AutoIncrement),
		Json:               getBool(sdm.E_Json),
		References:         getString(sdm.E_References),
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Per-field type & shape introspection
// ─────────────────────────────────────────────────────────────────────────────

// isProtoTimestamp reports whether a field is the well-known
// google.protobuf.Timestamp message. We special-case it so the Go side uses
// time.Time (not *Timestamppb / JSON bytes) and the DB column is a real
// `timestamp with time zone`, not JSONB.
func isProtoTimestamp(field *protogen.Field) bool {
	if field.Desc.Kind() != protoreflect.MessageKind {
		return false
	}
	if field.Message == nil {
		return false
	}
	return field.Message.Desc.FullName() == "google.protobuf.Timestamp"
}

// isJsonStored reports whether a field is materialized as JSON: either marked
// with (sdm.json)=true on a string field, or any message-kind field (which has
// no scalar SQL mapping and is always serialized as JSON). google.protobuf.Timestamp
// is excluded — it has a first-class SQL mapping (timestamptz).
func isJsonStored(field *protogen.Field, opts SdmOptions) bool {
	if isProtoTimestamp(field) {
		return false
	}
	return opts.Json || field.Desc.Kind() == protoreflect.MessageKind
}

// needsProtojsonMarshal reports whether a singular (non-list) message-kind field
// requires protojson.Marshal. Repeated message fields are handled separately by
// needsRepeatedProtojsonMarshal — they are slices, not proto.Message values.
// google.protobuf.Timestamp is excluded — it maps to time.Time via .AsTime().
func needsProtojsonMarshal(field *protogen.Field) bool {
	if isProtoTimestamp(field) {
		return false
	}
	return field.Desc.Kind() == protoreflect.MessageKind && !field.Desc.IsList()
}

// needsRepeatedProtojsonMarshal reports whether a repeated message-kind field
// requires element-wise protojson marshaling into a JSON array string.
// google.protobuf.Timestamp is excluded (repeated timestamps unsupported today).
func needsRepeatedProtojsonMarshal(field *protogen.Field) bool {
	if isProtoTimestamp(field) {
		return false
	}
	return field.Desc.Kind() == protoreflect.MessageKind && field.Desc.IsList()
}

// jsonVarName produces the local variable name used to hold the marshaled JSON
// bytes for a message-kind field, e.g. field.GoName=Amount → "amountJSON".
func jsonVarName(field *protogen.Field) string {
	return strings.ToLower(field.GoName[:1]) + field.GoName[1:] + "JSON"
}

// goTypeForField maps proto field kinds to Go types.
// Enum fields map to their Go type name (int32 alias) — NOT string.
// The View struct stores enums as string (chain field_value); the PII struct
// and proto model use the typed enum. goTypeForField is used for PII structs,
// proto model fields, and repo parameters — all of which want the typed enum.
func goTypeForField(field *protogen.Field) string {
	// google.protobuf.Timestamp → time.Time (not *Timestamppb).
	// Conversions happen at the model boundary via .AsTime() in Create.
	if isProtoTimestamp(field) {
		return "time.Time"
	}
	if field.Desc.Kind() == protoreflect.MessageKind {
		if field.Desc.IsList() {
			return "[]*" + field.Message.GoIdent.GoName
		}
		return "*" + field.Message.GoIdent.GoName
	}
	if field.Desc.Kind() == protoreflect.EnumKind {
		return field.Enum.GoIdent.GoName
	}
	if getFieldOptions(field).Json {
		return "datatypes.JSON"
	}
	base := func() string {
		switch field.Desc.Kind() {
		case protoreflect.StringKind:
			return "string"
		case protoreflect.Int64Kind, protoreflect.Int32Kind:
			return "int64"
		default:
			return "string"
		}
	}()
	if field.Desc.IsList() {
		return "[]" + base
	}
	return base
}

// goTypeForViewField is goTypeForField for View struct fields.
// Enum fields in the View are stored as string (chain field_value is TEXT),
// so the GORM scan target must be string. Everything else delegates to goTypeForField.
func goTypeForViewField(field *protogen.Field) string {
	if field.Desc.Kind() == protoreflect.EnumKind {
		return "string"
	}
	return goTypeForField(field)
}

// sqlTypeForField emits BIGSERIAL for auto_increment fields, TIMESTAMP WITH
// TIME ZONE for google.protobuf.Timestamp, JSONB for other json-stored fields,
// TEXT/BIGINT otherwise.
func sqlTypeForField(field *protogen.Field, opts SdmOptions) string {
	if opts.AutoIncrement {
		return "BIGSERIAL"
	}
	if isProtoTimestamp(field) {
		return "TIMESTAMP WITH TIME ZONE"
	}
	if isJsonStored(field, opts) {
		// Repeated messages and any (sdm.json)=true field — including
		// `repeated MessageType` in PII — store as a single JSONB column
		// (a JSON array of objects for the slice case).
		return "JSONB"
	}
	base := func() string {
		switch field.Desc.Kind() {
		case protoreflect.StringKind:
			return "TEXT"
		case protoreflect.Int64Kind, protoreflect.Int32Kind:
			return "BIGINT"
		default:
			return "TEXT"
		}
	}()
	if field.Desc.IsList() {
		// `repeated string` / `repeated intN` in PII → native Postgres array.
		return base + "[]"
	}
	return base
}

// ─────────────────────────────────────────────────────────────────────────────
// Per-message introspection
// ─────────────────────────────────────────────────────────────────────────────

// isSdmRecord reports whether a message should produce SDM tables.
// A record needs at least one primary_key OR chain_identifier_key field.
// Value-type messages (e.g. Money, RequiredDocument) used inline as JSON have
// neither and are skipped to avoid empty tables and broken views.
func isSdmRecord(msg *protogen.Message) bool {
	for _, field := range msg.Fields {
		opts := getFieldOptions(field)
		if opts.PrimaryKey || opts.ChainIdentifierKey {
			return true
		}
	}
	return false
}

// isChainOnly reports whether a message has no PII-routed fields (no primary_key,
// no pii-annotated fields). For chain-only messages the generator skips the PII
// table, the Create method, and FK constraints, and emits a pivot view over
// chain_* instead of a join-based view anchored on pii_*.
func isChainOnly(msg *protogen.Message) bool {
	for _, field := range msg.Fields {
		opts := getFieldOptions(field)
		if opts.PrimaryKey || opts.Pii {
			return false
		}
	}
	return true
}

// chainAcceptDefault is the chain-only accept predicate used outside a per-message
// closure — it matches the same logic as the chain-only chainAccept inside generateRepo.
func chainAcceptDefault(opts SdmOptions) bool {
	return !opts.ChainIdentifierKey
}

// chainKeyCols returns the columns used as the chain key for a message.
// Prefers (sdm.chain_identifier_key) fields; falls back to (sdm.primary_key)
// fields when no chain key is annotated.
func chainKeyCols(msg *protogen.Message) []string {
	var cols []string
	for _, field := range msg.Fields {
		if getFieldOptions(field).ChainIdentifierKey {
			cols = append(cols, string(field.Desc.Name()))
		}
	}
	if len(cols) > 0 {
		return cols
	}
	for _, field := range msg.Fields {
		if getFieldOptions(field).PrimaryKey {
			cols = append(cols, string(field.Desc.Name()))
		}
	}
	return cols
}

// chainKeyGoFields returns the protogen fields used as the chain key.
// Same preference order as chainKeyCols, returning *protogen.Field so callers
// can emit code that references the Go field.
func chainKeyGoFields(msg *protogen.Message) []*protogen.Field {
	var fields []*protogen.Field
	for _, field := range msg.Fields {
		if getFieldOptions(field).ChainIdentifierKey {
			fields = append(fields, field)
		}
	}
	if len(fields) > 0 {
		return fields
	}
	for _, field := range msg.Fields {
		if getFieldOptions(field).PrimaryKey {
			fields = append(fields, field)
		}
	}
	return fields
}

// sqlCompositeKeyExpr builds the SQL expression that reproduces the compositeKey.
// For chain-only messages the anchor alias is "keys"; for PII-backed messages it is "p".
func sqlCompositeKeyExpr(keyCols []string, tableAlias string) string {
	if len(keyCols) == 1 {
		return tableAlias + "." + keyCols[0]
	}
	parts := make([]string, len(keyCols))
	for i, col := range keyCols {
		parts[i] = tableAlias + "." + col
	}
	return strings.Join(parts, " || ':' || ")
}

// updatableSqlCols returns the SQL column names that Upsert/Update mutate on
// the PII table. Excludes: PK, auto_increment, chain_identifier_key (the
// natural lookup key is immutable), and audit columns the DB manages. Always
// appends "updated_at" so callers can rebump it on conflict. "created_at"
// and "created_by" are intentionally NOT in the update set — they're
// preserved from the original insert across upserts. There's no
// "updated_by" column; per-update actor lives in audit_pii_<name>s.
func updatableSqlCols(msg *protogen.Message) []string {
	var cols []string
	for _, f := range updatableGoFields(msg) {
		cols = append(cols, string(f.Desc.Name()))
	}
	cols = append(cols, "updated_at")
	return cols
}

// updatableGoFields returns the proto fields whose values flow into the
// Update/Upsert PII struct. Mirrors updatableSqlCols, but as *protogen.Fields
// so callers can emit `<GoName>: model.<GoName>`.
func updatableGoFields(msg *protogen.Message) []*protogen.Field {
	var fields []*protogen.Field
	for _, f := range msg.Fields {
		opts := getFieldOptions(f)
		if opts.PrimaryKey || opts.AutoIncrement || opts.ChainIdentifierKey {
			continue
		}
		if !(opts.Pii || opts.QueryIndex || opts.References != "") {
			continue
		}
		fields = append(fields, f)
	}
	return fields
}

// ─────────────────────────────────────────────────────────────────────────────
// File-level predicates — used by the per-file generators to decide what
// imports / serializers / column types they need to emit.
// ─────────────────────────────────────────────────────────────────────────────

// fileHasStringJsonField reports whether any string field is annotated with
// (sdm.json)=true.
func fileHasStringJsonField(file *protogen.File) bool {
	for _, msg := range file.Messages {
		if !isSdmRecord(msg) {
			continue
		}
		for _, field := range msg.Fields {
			opts := getFieldOptions(field)
			if opts.Json && field.Desc.Kind() == protoreflect.StringKind {
				return true
			}
		}
	}
	return false
}

// fileHasMessageJsonField reports whether any field is a nested proto message
// on a recorded message — drives the protojson serializer emission in helpers.
func fileHasMessageJsonField(file *protogen.File) bool {
	for _, msg := range file.Messages {
		if !isSdmRecord(msg) {
			continue
		}
		for _, field := range msg.Fields {
			if needsProtojsonMarshal(field) {
				return true
			}
		}
	}
	return false
}

// fileHasChainMessageField reports whether any chain-stored (non-PII, non-PK)
// field is a nested proto message.
func fileHasChainMessageField(file *protogen.File) bool {
	for _, msg := range file.Messages {
		if !isSdmRecord(msg) {
			continue
		}
		for _, field := range msg.Fields {
			if !needsProtojsonMarshal(field) {
				continue
			}
			opts := getFieldOptions(field)
			if !opts.Pii && !opts.PrimaryKey && opts.References == "" {
				return true
			}
		}
	}
	return false
}

// fileHasRepeatedMessageField reports whether any chain-stored field is a
// repeated message — drives the "strings" import in the repo file for the
// strings.Join call in the element-wise JSON array marshaling loop.
func fileHasRepeatedMessageField(file *protogen.File) bool {
	for _, msg := range file.Messages {
		if !isSdmRecord(msg) {
			continue
		}
		for _, field := range msg.Fields {
			if needsRepeatedProtojsonMarshal(field) {
				opts := getFieldOptions(field)
				if chainAcceptDefault(opts) {
					return true
				}
			}
		}
	}
	return false
}

// fileHasTimestampField reports whether any field on any recorded message in
// the file is google.protobuf.Timestamp — regardless of PII vs chain placement.
// Drives the timestamppb import in the model file for AsBaseModel.
func fileHasTimestampField(file *protogen.File) bool {
	for _, msg := range file.Messages {
		if !isSdmRecord(msg) {
			continue
		}
		for _, field := range msg.Fields {
			if isProtoTimestamp(field) {
				return true
			}
		}
	}
	return false
}

// fileHasPiiBackedMessage reports whether any recorded message in the file
// has a PII table (i.e. is not chain-only). PII-backed messages generate the
// PiiAudit struct (which uses datatypes.JSON), so the model file's
// `gorm.io/datatypes` import must be present.
func fileHasPiiBackedMessage(file *protogen.File) bool {
	for _, msg := range file.Messages {
		if !isSdmRecord(msg) {
			continue
		}
		if !isChainOnly(msg) {
			return true
		}
	}
	return false
}

// fileHasChainRepeatedMessageField reports whether any chain-stored repeated
// message field exists on a recorded message. PII-stored repeated messages
// are excluded — those go through the protojsonArray serializer in
// sdm_helpers.go, not the repo's chain marshal loop.
func fileHasChainRepeatedMessageField(file *protogen.File) bool {
	for _, msg := range file.Messages {
		if !isSdmRecord(msg) {
			continue
		}
		chainOnly := isChainOnly(msg)
		for _, field := range msg.Fields {
			if !needsRepeatedProtojsonMarshal(field) {
				continue
			}
			opts := getFieldOptions(field)
			if chainOnly {
				if !opts.ChainIdentifierKey {
					return true
				}
			} else {
				if !opts.PrimaryKey && !opts.Pii && opts.References == "" {
					return true
				}
			}
		}
	}
	return false
}

// fileHasPiiRepeatedScalarField reports whether any PII-stored repeated scalar
// field exists on a recorded message. Drives the `github.com/lib/pq` import
// in the generated repo for the `pq.StringArray(model.X)` cast in the PII
// struct literal.
func fileHasPiiRepeatedScalarField(file *protogen.File) bool {
	for _, msg := range file.Messages {
		if !isSdmRecord(msg) {
			continue
		}
		for _, field := range msg.Fields {
			if !field.Desc.IsList() {
				continue
			}
			if field.Desc.Kind() == protoreflect.MessageKind {
				continue
			}
			opts := getFieldOptions(field)
			if opts.Pii || opts.QueryIndex || opts.References != "" {
				return true
			}
		}
	}
	return false
}

// fileHasChainTimestampField reports whether any chain-stored field is a
// google.protobuf.Timestamp. Drives the `time` import in the generated repo
// for the RFC3339Nano chain-serialization expression.
func fileHasChainTimestampField(file *protogen.File) bool {
	for _, msg := range file.Messages {
		if !isSdmRecord(msg) {
			continue
		}
		for _, field := range msg.Fields {
			if !isProtoTimestamp(field) {
				continue
			}
			opts := getFieldOptions(field)
			if !opts.Pii && !opts.PrimaryKey && opts.References == "" {
				return true
			}
		}
	}
	return false
}

// fileHasPiiStringJsonField reports whether any PII-routed string field is
// annotated (sdm.json)=true.
func fileHasPiiStringJsonField(file *protogen.File) bool {
	for _, msg := range file.Messages {
		if !isSdmRecord(msg) {
			continue
		}
		for _, field := range msg.Fields {
			opts := getFieldOptions(field)
			if opts.Pii && opts.Json && field.Desc.Kind() == protoreflect.StringKind {
				return true
			}
		}
	}
	return false
}
