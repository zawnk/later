package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func lastIDPath() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locating cache dir: %w", err)
	}
	return filepath.Join(dir, "later", "last_id"), nil
}

func saveLastID(id string) {
	path, err := lastIDPath()
	if err == nil {
		if err = os.MkdirAll(filepath.Dir(path), 0700); err == nil {
			err = os.WriteFile(path, []byte(id+"\n"), 0600)
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not cache reminder id: %v\n", err)
	}
}

func resolveID(arg string) (string, error) {
	if arg != "last" {
		return arg, nil
	}
	path, err := lastIDPath()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", errors.New(`no cached reminder id yet -- "last" works after this CLI has created a reminder`)
		}
		return "", fmt.Errorf("reading cached reminder id: %w", err)
	}
	id := strings.TrimSpace(string(data))
	if id == "" {
		return "", errors.New("cached reminder id file is empty")
	}
	return id, nil
}
