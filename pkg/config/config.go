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
}

// AuditTablesEnabled returns the effective value of CreateAuditTables,
// defaulting to true when unset.
func (c *Config) AuditTablesEnabled() bool {
	if c.CreateAuditTables == nil {
		return true
	}
	return *c.CreateAuditTables
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
