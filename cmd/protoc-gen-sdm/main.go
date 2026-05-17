package main

import (
	"flag"

	"github.com/kapow-tech/sdm/pkg/generator"
	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/types/pluginpb"
)

func main() {
	var flags flag.FlagSet
	// Defaults mirror the YAML defaults. Override with --sdm_opt=KEY=VALUE
	// when invoking protoc-gen-sdm directly (e.g. from buf.gen.yaml).
	createAuditTables := flags.Bool("create-audit-tables", true,
		"emit audit_pii_<name>s tables, trigger, struct, and Repo.AuditLog method")
	chainDrafts := flags.Bool("chain-drafts", false,
		"emit chain status (DRAFTED/CREATED/DROPPED), DraftChain/CommitChain/DropChain, Upsert/Update; skip SaveAll/SaveChain")
	protogen.Options{
		ParamFunc: flags.Set,
	}.Run(func(gen *protogen.Plugin) error {
		gen.SupportedFeatures = uint64(pluginpb.CodeGeneratorResponse_FEATURE_PROTO3_OPTIONAL)
		opts := generator.Options{
			CreateAuditTables: *createAuditTables,
			ChainDrafts:       *chainDrafts,
		}
		for _, f := range gen.Files {
			if !f.Generate {
				continue
			}
			generator.GenerateFile(gen, f, opts)
		}
		return nil
	})
}
