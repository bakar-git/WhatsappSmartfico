package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/joho/godotenv"
)

// Prefer configuration beside the installed executable. The working-directory
// fallback lets go run use the project's .env instead of its temporary build dir.
func loadDotEnv(executable, workingDir string) (string, error) {
	paths := []string{filepath.Join(filepath.Dir(executable), ".env"), filepath.Join(workingDir, ".env")}
	for index, path := range paths {
		if index > 0 && path == paths[0] {
			continue
		}
		values, err := godotenv.Read(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			// Parser errors may include the offending line, which could contain a
			// secret. Report the path without logging the file's contents.
			return "", fmt.Errorf("cannot load %s: invalid or unreadable .env file", path)
		}
		for key, value := range values {
			if _, exists := os.LookupEnv(key); !exists {
				if err := os.Setenv(key, value); err != nil {
					return "", fmt.Errorf("cannot set .env variable %s", key)
				}
			}
		}
		return filepath.Dir(path), nil
	}
	return workingDir, nil
}
