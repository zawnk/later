package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/alecthomas/kong"
)

func configFilePath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locating config dir: %w", err)
	}
	return filepath.Join(dir, "later", "config"), nil
}

func loadConfigFile(path string) (map[string]string, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	if info.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(os.Stderr, "warning: %s is readable by other users (mode %04o) -- consider: chmod 600 %s\n", path, info.Mode().Perm(), path)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	values := map[string]string{}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: not a key=value line: %q", path, i+1, line)
		}
		key = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(key)), "later_")
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		switch key {
		case "url", "token":
			values[key] = value
		default:
			return nil, fmt.Errorf("%s:%d: unknown key %q (known keys: url, token)", path, i+1, key)
		}
	}
	return values, nil
}

func configFileResolver() (kong.Resolver, error) {
	path, err := configFilePath()
	if err != nil {
		return kong.ResolverFunc(func(*kong.Context, *kong.Path, *kong.Flag) (any, error) {
			return nil, nil
		}), nil
	}
	values, err := loadConfigFile(path)
	if err != nil {
		return nil, err
	}
	return kong.ResolverFunc(func(_ *kong.Context, _ *kong.Path, flag *kong.Flag) (any, error) {
		for _, env := range flag.Envs {
			if _, ok := os.LookupEnv(env); ok {
				return nil, nil
			}
		}
		if v, ok := values[flag.Name]; ok {
			return v, nil
		}
		return nil, nil
	}), nil
}
