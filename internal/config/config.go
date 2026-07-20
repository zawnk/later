package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

const minTokenLength = 16

type Config struct {
	Server     ServerConfig `yaml:"server"`
	Ntfy       NtfyConfig   `yaml:"ntfy"`
	Inbound    []Inbound    `yaml:"inbound"`
	AuthTokens []Token      `yaml:"auth_tokens"`
	LatePrefix string       `yaml:"late_prefix"`
}

type ServerConfig struct {
	Port    int    `yaml:"port"`
	BaseURL string `yaml:"base_url"`
	DataDir string `yaml:"data_dir"`
}

type NtfyConfig struct {
	Server string `yaml:"server"`
	Token  string `yaml:"token"`
}

type Inbound struct {
	Topic    string   `yaml:"topic"`
	Outbound []string `yaml:"outbound"`
}

type Token struct {
	Token           string   `yaml:"token"`
	Outbound        []string `yaml:"outbound"`
	DefaultOutbound string   `yaml:"default_outbound"`
}

func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("error when opening config: %w", err)
	}
	defer f.Close()

	var cfg Config
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("error parsing config %s: %w", path, err)
	}

	cfg.applyDefaults()

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Server.Port == 0 {
		c.Server.Port = 8080
	}
	if c.Server.DataDir == "" {
		c.Server.DataDir = "/data"
	}
	if c.LatePrefix == "" {
		c.LatePrefix = "DELAYED:"
	}
}

func (c *Config) validate() error {
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port: must be 1-65535, got %d", c.Server.Port)
	}

	if strings.TrimSpace(c.Ntfy.Server) == "" {
		return errors.New("ntfy.server is required")
	}
	if _, err := url.Parse(c.Ntfy.Server); err != nil {
		return fmt.Errorf("ntfy.server is not a valid URL: %w", err)
	}

	if strings.TrimSpace(c.Ntfy.Token) == "" {
		return errors.New("ntfy.token is required")
	}

	if len(c.AuthTokens) == 0 && len(c.Inbound) == 0 {
		return errors.New("configure at least one of auth_tokens or inbound; otherwise no way to create reminders")
	}

	seenTokens := make(map[string]int, len(c.AuthTokens))
	for i, t := range c.AuthTokens {
		if len(t.Token) < minTokenLength {
			return fmt.Errorf("auth_tokens[%d].token: must be at least %d chars", i, minTokenLength)
		}
		if prev, dup := seenTokens[t.Token]; dup {
			return fmt.Errorf("auth_tokens[%d].token: duplicate of auth_tokens[%d]", i, prev)
		}
		seenTokens[t.Token] = i

		if len(t.Outbound) == 0 {
			return fmt.Errorf("auth_tokens[%d].outbound: required (topics this token may publish to)", i)
		}
		if t.DefaultOutbound != "" && !slices.Contains(t.Outbound, t.DefaultOutbound) {
			return fmt.Errorf("auth_tokens[%d].default_outbound %q: must be one of the token's outbound topics", i, t.DefaultOutbound)
		}
	}

	seenTopics := make(map[string]int, len(c.Inbound))
	for i, in := range c.Inbound {
		if strings.TrimSpace(in.Topic) == "" {
			return fmt.Errorf("inbound[%d].topic: required", i)
		}
		if prev, dup := seenTopics[in.Topic]; dup {
			return fmt.Errorf("inbound[%d].topic %q: duplicate of inbound[%d]", i, in.Topic, prev)
		}
		seenTopics[in.Topic] = i

		if len(in.Outbound) == 0 {
			return fmt.Errorf("inbound[%d].outbound: required (topics reminders from %q are published to)", i, in.Topic)
		}
	}

	for i, t := range c.AuthTokens {
		for _, out := range t.Outbound {
			if _, isInbound := seenTopics[out]; isInbound {
				return fmt.Errorf("auth_tokens[%d].outbound %q: also configured as an inbound topic (would create a notification loop)", i, out)
			}
		}
	}
	for i, in := range c.Inbound {
		for _, out := range in.Outbound {
			if _, isInbound := seenTopics[out]; isInbound {
				return fmt.Errorf("inbound[%d].outbound %q: also configured as an inbound topic (would create a notification loop)", i, out)
			}
		}
	}

	return nil
}
