package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizedBaseURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"already absolute, unchanged", "https://later.example.com", "https://later.example.com"},
		{"bare IP:port gets http:// prepended", "192.168.1.53:8080", "http://192.168.1.53:8080"},
		{"bare hostname:port gets http:// prepended", "later-server:8080", "http://later-server:8080"},
		{"bare hostname with no port is left unchanged (ambiguous, not our call to guess)", "later.example.com", "later.example.com"},
		{"scheme with no host is left unchanged (still invalid, not our job to fix)", "https://", "https://"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizedBaseURL(tt.in); got != tt.want {
				t.Errorf("NormalizedBaseURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestApplyDefaults(t *testing.T) {
	tests := []struct {
		name        string
		in          Config
		wantPort    int
		wantDataDir string
		wantPrefix  string
	}{
		{
			name:        "all zero values get defaults",
			in:          Config{},
			wantPort:    8080,
			wantDataDir: "/data",
			wantPrefix:  "DELAYED:",
		},
		{
			name:        "explicit values are all preserved",
			in:          Config{Server: ServerConfig{Port: 9090, DataDir: "/custom"}, LatePrefix: "LATE:"},
			wantPort:    9090,
			wantDataDir: "/custom",
			wantPrefix:  "LATE:",
		},
		{
			name:        "defaults apply independently per field",
			in:          Config{Server: ServerConfig{Port: 9090}},
			wantPort:    9090,
			wantDataDir: "/data",
			wantPrefix:  "DELAYED:",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.in
			cfg.applyDefaults()

			if cfg.Server.Port != tt.wantPort {
				t.Errorf("Server.Port = %d, want %d", cfg.Server.Port, tt.wantPort)
			}

			if cfg.Server.DataDir != tt.wantDataDir {
				t.Errorf("Server.DataDir = %q, want %q", cfg.Server.DataDir, tt.wantDataDir)
			}

			if cfg.LatePrefix != tt.wantPrefix {
				t.Errorf("LatePrefix = %q, want %q", cfg.LatePrefix, tt.wantPrefix)
			}
		})
	}
}

func validConfig() Config {
	return Config{
		Server: ServerConfig{Port: 8080, DataDir: "/data"},
		Ntfy:   NtfyConfig{Server: "https://ntfy.sh", Token: "tk_sometoken"},
		AuthTokens: []Token{
			{Token: "0123456789abcdef", Outbound: []string{"topic-a"}},
		},
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(c *Config)
		wantErr string
	}{
		{"baseline config is valid", func(c *Config) {}, ""},

		{"port 0 is invalid", func(c *Config) { c.Server.Port = 0 }, "server.port"},
		{"port above 65535 is invalid", func(c *Config) { c.Server.Port = 70000 }, "server.port"},
		{"port negative is invalid", func(c *Config) { c.Server.Port = -1 }, "server.port"},
		{"port 1 is valid (lower boundary)", func(c *Config) { c.Server.Port = 1 }, ""},
		{"port 65535 is valid (upper boundary)", func(c *Config) { c.Server.Port = 65535 }, ""},

		{"missing ntfy.server", func(c *Config) { c.Ntfy.Server = "" }, "ntfy.server is required"},
		{"whitespace-only ntfy.server", func(c *Config) { c.Ntfy.Server = "   " }, "ntfy.server is required"},
		{"missing ntfy.token", func(c *Config) { c.Ntfy.Token = "" }, "ntfy.token is required"},

		{
			name:    "ntfy.server with no scheme is accepted",
			mutate:  func(c *Config) { c.Ntfy.Server = "just-a-hostname-no-scheme" },
			wantErr: "",
		},
		{
			name:    "ntfy.server with an invalid URL is rejected",
			mutate:  func(c *Config) { c.Ntfy.Server = "http://ex\x7fample.com" },
			wantErr: "not a valid URL",
		},

		{"empty base_url is fine (feature just stays off)", func(c *Config) { c.Server.BaseURL = "" }, ""},
		{"base_url with scheme and host is valid", func(c *Config) { c.Server.BaseURL = "https://later.example.com" }, ""},
		{"base_url with IP and port is valid", func(c *Config) { c.Server.BaseURL = "192.168.1.53:8080" }, ""},
		{"base_url with no scheme is rejected", func(c *Config) { c.Server.BaseURL = "later.example.com" }, "server.base_url is not an absolute URL"},
		{"base_url with scheme but no host is rejected", func(c *Config) { c.Server.BaseURL = "https://" }, "server.base_url is not an absolute URL"},

		{"no auth_tokens and no inbound", func(c *Config) { c.AuthTokens = nil }, "configure at least one"},
		{
			name: "inbound alone (no auth_tokens) is sufficient",
			mutate: func(c *Config) {
				c.AuthTokens = nil
				c.Inbound = []Inbound{{Topic: "inbound-topic", Outbound: []string{"out-topic"}}}
			},
			wantErr: "",
		},

		{"auth token shorter than minTokenLength", func(c *Config) { c.AuthTokens[0].Token = "short" }, "must be at least 16 chars"},
		{
			name: "duplicate auth tokens",
			mutate: func(c *Config) {
				c.AuthTokens = append(c.AuthTokens, Token{Token: c.AuthTokens[0].Token, Outbound: []string{"topic-a"}})
			},
			wantErr: "duplicate",
		},

		{
			name:    "token without outbound topics is rejected",
			mutate:  func(c *Config) { c.AuthTokens[0].Outbound = nil },
			wantErr: "outbound: required",
		},
		{
			name:    "token default_outbound within its outbound list is valid",
			mutate:  func(c *Config) { c.AuthTokens[0].DefaultOutbound = "topic-a" },
			wantErr: "",
		},
		{
			name:    "token default_outbound outside its outbound list is rejected",
			mutate:  func(c *Config) { c.AuthTokens[0].DefaultOutbound = "not-in-list" },
			wantErr: "must be one of the token's outbound topics",
		},

		{
			name:    "inbound topic empty",
			mutate:  func(c *Config) { c.Inbound = []Inbound{{Topic: ""}} },
			wantErr: "inbound[0].topic: required",
		},
		{
			name:    "inbound topic whitespace-only",
			mutate:  func(c *Config) { c.Inbound = []Inbound{{Topic: "   "}} },
			wantErr: "inbound[0].topic: required",
		},
		{
			name: "duplicate inbound topics",
			mutate: func(c *Config) {
				c.Inbound = []Inbound{
					{Topic: "same", Outbound: []string{"out-topic"}},
					{Topic: "same", Outbound: []string{"out-topic"}},
				}
			},
			wantErr: "duplicate",
		},
		{
			name:    "inbound without outbound topics is rejected",
			mutate:  func(c *Config) { c.Inbound = []Inbound{{Topic: "inbound-topic"}} },
			wantErr: "inbound[0].outbound: required",
		},

		{
			name: "token outbound overlapping an inbound topic is rejected",
			mutate: func(c *Config) {
				c.Inbound = []Inbound{{Topic: "inbound-topic", Outbound: []string{"out-topic"}}}
				c.AuthTokens[0].Outbound = []string{"inbound-topic"}
			},
			wantErr: "would create a notification loop",
		},
		{
			name: "inbound outbound overlapping another inbound topic is rejected",
			mutate: func(c *Config) {
				c.Inbound = []Inbound{
					{Topic: "inbound-a", Outbound: []string{"inbound-b"}},
					{Topic: "inbound-b", Outbound: []string{"out-topic"}},
				}
			},
			wantErr: "would create a notification loop",
		},
		{
			name: "inbound topic feeding back into itself is rejected",
			mutate: func(c *Config) {
				c.Inbound = []Inbound{{Topic: "inbound-topic", Outbound: []string{"inbound-topic"}}}
			},
			wantErr: "would create a notification loop",
		},

		{
			name:    "duplicate topic within a token's outbound list is rejected",
			mutate:  func(c *Config) { c.AuthTokens[0].Outbound = []string{"topic-a", "topic-a"} },
			wantErr: "duplicate topic",
		},
		{
			name: "duplicate topic within an inbound's outbound list is rejected",
			mutate: func(c *Config) {
				c.Inbound = []Inbound{{Topic: "inbound-topic", Outbound: []string{"out-topic", "out-topic"}}}
			},
			wantErr: "duplicate topic",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.mutate(&cfg)

			err := cfg.validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validate() error = %v, want nil", err)
				}
				return
			}

			if err == nil {
				t.Fatalf("validate() error = nil, want error containing %q", tt.wantErr)
			}

			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("validate() error = %q, want it to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatalf("failed to write test config: %v", err)
	}
	return path
}

func TestLoad(t *testing.T) {
	t.Run("valid minimal config loads with defaults applied", func(t *testing.T) {
		path := writeConfig(t, `
ntfy:
  server: https://ntfy.sh
  token: tk_sometoken
auth_tokens:
  - token: "0123456789abcdef"
    outbound: ["topic-a"]
`)
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}

		if cfg.Server.Port != 8080 {
			t.Errorf("Server.Port = %d, want default 8080", cfg.Server.Port)
		}

		if cfg.Server.DataDir != "/data" {
			t.Errorf("Server.DataDir = %q, want default /data", cfg.Server.DataDir)
		}

		if cfg.LatePrefix != "DELAYED:" {
			t.Errorf("LatePrefix = %q, want default DELAYED:", cfg.LatePrefix)
		}
	})

	t.Run("missing file returns an error", func(t *testing.T) {
		_, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
		if err == nil {
			t.Fatal("Load() error = nil, want an error for a missing file")
		}
	})

	t.Run("malformed YAML returns an error", func(t *testing.T) {
		path := writeConfig(t, "ntfy: [this is not, valid: yaml")
		_, err := Load(path)
		if err == nil {
			t.Fatal("Load() error = nil, want a YAML parse error")
		}
	})

	t.Run("unknown field is rejected", func(t *testing.T) {
		path := writeConfig(t, `
ntfy:
  server: https://ntfy.sh
  toekn: tk_sometoken
`)
		_, err := Load(path)
		if err == nil {
			t.Fatal("Load() error = nil, want an error for the misspelled key 'toekn'")
		}
	})

	t.Run("config that fails validate() surfaces the error", func(t *testing.T) {
		path := writeConfig(t, `
ntfy:
  server: https://ntfy.sh
`)
		_, err := Load(path)
		if err == nil {
			t.Fatal("Load() error = nil, want a validation error")
		}
	})
}
