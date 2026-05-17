package config

import (
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	SdmVersion string   `yaml:"sdm"`
	SdmProto   string   `yaml:"sdm-proto"`
	Source     string   `yaml:"source"`
	UserProtos []string `yaml:"user-protos"`
	Output     string   `yaml:"output"`
	OutputSQL  string   `yaml:"output-sql"`
	// CreateAuditTables toggles emission of audit_pii_<name>s tables, the
	// AFTER UPDATE/DELETE trigger, the <Name>PiiAudit Go struct, and the
	// Repo.AuditLog method. Pointer so a missing YAML key defaults to true
	// (back-compat with configs written before this knob existed).
	CreateAuditTables *bool `yaml:"create-audit-tables"`
	// ChainDrafts toggles the draft/commit workflow on chain rows. When true,
	// each chain row carries a status (DRAFTED / CREATED / DROPPED), a partial
	// unique index allows at most one DRAFTED row per (key, field_name), a
	// BEFORE UPDATE trigger enforces legal transitions, and two views are
	// emitted (committed-only and with-drafts). The generator emits
	// DraftChain / CommitChain / DropChain methods plus Upsert / Update
	// (instead of SaveAll / SaveChain). Pointer so a missing YAML key
	// defaults to false (back-compat with configs written before this knob).
	ChainDrafts *bool `yaml:"chain-drafts"`
}

// AuditTablesEnabled returns the effective value of CreateAuditTables,
// defaulting to true when unset.
func (c *Config) AuditTablesEnabled() bool {
	if c.CreateAuditTables == nil {
		return true
	}
	return *c.CreateAuditTables
}

// ChainDraftsEnabled returns the effective value of ChainDrafts,
// defaulting to false when unset (the draft workflow is opt-in).
func (c *Config) ChainDraftsEnabled() bool {
	if c.ChainDrafts == nil {
		return false
	}
	return *c.ChainDrafts
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	err = yaml.Unmarshal(data, &cfg)
	if err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) Save(path string) error {
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}
