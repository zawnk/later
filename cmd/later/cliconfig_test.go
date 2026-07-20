package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
)

func writeCLIConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	path := filepath.Join(dir, "later", "config")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func unsetEnv(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	_ = os.Unsetenv(key)
}

func TestLoadConfigFile(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		wantURL   string
		wantToken string
		wantErr   string
	}{
		{
			name:      "plain lowercase keys",
			content:   "url=http://homelab:8080\ntoken=tk_abc\n",
			wantURL:   "http://homelab:8080",
			wantToken: "tk_abc",
		},
		{
			name:      "uppercase and LATER_-prefixed keys",
			content:   "TOKEN=tk_abc\nLATER_URL=http://homelab:8080\n",
			wantURL:   "http://homelab:8080",
			wantToken: "tk_abc",
		},
		{
			name:      "comments, blank lines, spaces around = and quoted values",
			content:   "# my later config\n\n  url = \"http://homelab:8080\"  \ntoken = 'tk_abc'\n",
			wantURL:   "http://homelab:8080",
			wantToken: "tk_abc",
		},
		{
			name:    "unknown key fails loudly with the line number",
			content: "url=http://x\ntoken2=tk_abc\n",
			wantErr: ":2: unknown key",
		},
		{
			name:    "line without = fails loudly",
			content: "just some words\n",
			wantErr: "not a key=value line",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeCLIConfig(t, tt.content)
			values, err := loadConfigFile(path)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("loadConfigFile() error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadConfigFile() error = %v", err)
			}
			if values["url"] != tt.wantURL {
				t.Errorf("url = %q, want %q", values["url"], tt.wantURL)
			}
			if values["token"] != tt.wantToken {
				t.Errorf("token = %q, want %q", values["token"], tt.wantToken)
			}
		})
	}
}

func TestLoadConfigFile_Missing(t *testing.T) {
	values, err := loadConfigFile(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("loadConfigFile() error = %v, want nil for a missing file", err)
	}
	if values != nil {
		t.Errorf("loadConfigFile() = %v, want nil", values)
	}
}

func TestLoadConfigFile_ExampleParses(t *testing.T) {
	values, err := loadConfigFile("config.example")
	if err != nil {
		t.Fatalf("loadConfigFile(config.example) error = %v", err)
	}
	if values["url"] != "https://later.domain.com" {
		t.Errorf(`config.example url = %q, want "https://later.domain.com"`, values["url"])
	}
	if values["token"] != "tk_replace_this_with_a_real_token_at_least_16_chars" {
		t.Errorf("config.example token = %q, want the placeholder token", values["token"])
	}
}

func parseWithConfigFile(t *testing.T, args ...string) *CLI {
	t.Helper()
	resolver, err := configFileResolver()
	if err != nil {
		t.Fatalf("configFileResolver() error = %v", err)
	}
	var cli CLI
	parser, err := kong.New(&cli, kong.Name("later"), kong.Resolvers(resolver))
	if err != nil {
		t.Fatalf("kong.New() error = %v", err)
	}
	if _, err := parser.Parse(args); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	return &cli
}

func TestConfigFilePrecedence(t *testing.T) {
	t.Run("file fills in when env is unset", func(t *testing.T) {
		writeCLIConfig(t, "url=http://from-file:1234\ntoken=tk_from_file\n")
		unsetEnv(t, "LATER_URL")
		unsetEnv(t, "LATER_TOKEN")

		cli := parseWithConfigFile(t, "next")
		if cli.URL != "http://from-file:1234" {
			t.Errorf("URL = %q, want the config-file value", cli.URL)
		}
		if cli.Token != "tk_from_file" {
			t.Errorf("Token = %q, want the config-file value", cli.Token)
		}
	})

	t.Run("env beats file", func(t *testing.T) {
		writeCLIConfig(t, "url=http://from-file:1234\ntoken=tk_from_file\n")
		t.Setenv("LATER_URL", "http://from-env:5678")
		unsetEnv(t, "LATER_TOKEN")

		cli := parseWithConfigFile(t, "next")
		if cli.URL != "http://from-env:5678" {
			t.Errorf("URL = %q, want the env value to beat the file", cli.URL)
		}
		if cli.Token != "tk_from_file" {
			t.Errorf("Token = %q, want the file value for the var that is NOT in the env", cli.Token)
		}
	})

	t.Run("flag beats file and env", func(t *testing.T) {
		writeCLIConfig(t, "url=http://from-file:1234\n")
		t.Setenv("LATER_URL", "http://from-env:5678")

		cli := parseWithConfigFile(t, "--url=http://from-flag:9999", "next")
		if cli.URL != "http://from-flag:9999" {
			t.Errorf("URL = %q, want the explicit flag to win", cli.URL)
		}
	})

	t.Run("no file at all leaves defaults intact", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // empty dir: no later/config
		unsetEnv(t, "LATER_URL")
		unsetEnv(t, "LATER_TOKEN")

		cli := parseWithConfigFile(t, "next")
		if cli.URL != "http://localhost:8080" {
			t.Errorf("URL = %q, want the struct default", cli.URL)
		}
		if cli.Token != "" {
			t.Errorf("Token = %q, want empty", cli.Token)
		}
	})
}
