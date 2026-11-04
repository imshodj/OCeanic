// Package agent implements the OCeanic endpoint agent.
package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/imshodj/oceanic/internal/protocol"
	"github.com/imshodj/oceanic/internal/trailer"
)

const ServiceName = "OCeanicAgent"
const ServiceDisplayName = "OCeanic RMM Agent"
const ServiceDescription = "Remote monitoring & management agent for OCeanic (outbound HTTPS only)."

// Config is persisted on disk after installation.
type Config struct {
	ServerURL            string `json:"server_url"`
	EnrollToken          string `json:"enroll_token,omitempty"`
	OrgName              string `json:"org_name,omitempty"`
	AgentID              string `json:"agent_id,omitempty"`
	AgentSecret          string `json:"agent_secret,omitempty"`
	CFAccessClientID     string `json:"cf_access_client_id,omitempty"`
	CFAccessClientSecret string `json:"cf_access_client_secret,omitempty"`
}

func (c *Config) Enrolled() bool { return c.AgentID != "" && c.AgentSecret != "" }

// LoadConfig reads the installed config file.
func LoadConfig() (*Config, error) {
	b, err := os.ReadFile(ConfigPath())
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	c.ServerURL = strings.TrimRight(c.ServerURL, "/")
	return &c, nil
}

// Save writes the config with restrictive permissions.
func (c *Config) Save() error {
	if err := os.MkdirAll(filepath.Dir(ConfigPath()), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	tmp := ConfigPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, ConfigPath()); err != nil {
		return err
	}
	return restrictConfigACL(ConfigPath())
}

// EmbeddedConfig reads the config the server appended to this executable.
func EmbeddedConfig() (*protocol.EmbeddedConfig, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	var ec protocol.EmbeddedConfig
	if err := trailer.Read(exe, &ec); err != nil {
		return nil, err
	}
	if ec.ServerURL == "" || ec.EnrollToken == "" {
		return nil, errors.New("embedded config incomplete")
	}
	return &ec, nil
}
