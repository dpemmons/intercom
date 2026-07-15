package codex

import (
	"path/filepath"
	"strings"
	"testing"
)

func validConfigForNormalization(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	return Config{
		Name:              "reviewer",
		Version:           "test",
		CWD:               dir,
		AppServerEndpoint: "unix:///tmp/intercom-app.sock",
		BrokerSocket:      filepath.Join(dir, "broker.sock"),
		StatePath:         filepath.Join(dir, "state.json"),
		LockPath:          filepath.Join(dir, "state.lock"),
	}
}

func TestNormalizeConfigSelectionAndPolicyContracts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{name: "new and adopt", mutate: func(c *Config) { c.New = true; c.AdoptThreadID = "thread" }, want: "mutually exclusive"},
		{name: "adopt and fork", mutate: func(c *Config) { c.AdoptThreadID = "one"; c.ForkThreadID = "two" }, want: "mutually exclusive"},
		{name: "replace alone", mutate: func(c *Config) { c.ReplaceBinding = true }, want: "requires --adopt-session or --fork-session"},
		{name: "invalid policy", mutate: func(c *Config) { c.ExecutionPolicy = "unconfined-ish" }, want: "unsupported execution policy"},
		{name: "relative bridge", mutate: func(c *Config) { c.MCPBridgeSocket = "relative.sock" }, want: "must be absolute"},
		{name: "relative executable", mutate: func(c *Config) { c.IntercomBin = "intercom" }, want: "executable path must be absolute"},
		{name: "adopt without bridge", mutate: func(c *Config) { c.AdoptThreadID = "thread" }, want: "require the managed MCP bridge"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := validConfigForNormalization(t)
			tt.mutate(&cfg)
			if _, err := normalizeConfig(cfg); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("normalizeConfig() error = %v, want fragment %q", err, tt.want)
			}
		})
	}
}

func TestNormalizeConfigDefaultsToWorkspaceWrite(t *testing.T) {
	t.Parallel()
	cfg, err := normalizeConfig(validConfigForNormalization(t))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ExecutionPolicy != ExecutionWorkspaceWrite {
		t.Fatalf("ExecutionPolicy = %q", cfg.ExecutionPolicy)
	}
}
