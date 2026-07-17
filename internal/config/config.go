package config

import (
	"os"

	"gopkg.in/yaml.v3"
)

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
	Server          string `yaml:"server"`
	Token           string `yaml:"token"`
	DefaultOutbound string `yaml:"default_outbound"`
}

type Inbound struct {
	Topic    string   `yaml:"topic"`
	Outbound []string `yaml:"outbound"`
}

type Token struct {
	Token    string   `yaml:"token"`
	Outbound []string `yaml:"outbound"`
}

func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var cfg Config
	if err := yaml.NewDecoder(f).Decode(&cfg); err != nil {
		// TODO: proper error handling
		return nil, err
	}
	return &cfg, nil
}
