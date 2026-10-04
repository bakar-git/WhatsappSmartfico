package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func unsetEnvForTest(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}

func TestDotEnvBesideExecutable(t *testing.T) {
	installDir, workingDir := t.TempDir(), t.TempDir()
	key := "SMARTFICO_DOTENV_TEST_VALUE"
	unsetEnvForTest(t, key)
	if err := os.WriteFile(filepath.Join(installDir, ".env"), []byte(key+`="installed value" # comment`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workingDir, ".env"), []byte(key+"=working-directory-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	dir, err := loadDotEnv(filepath.Join(installDir, "smartfico-whatsapp"), workingDir)
	if err != nil || dir != installDir || os.Getenv(key) != "installed value" {
		t.Fatalf("%s %q %v", dir, os.Getenv(key), err)
	}
}

func TestDotEnvWorkingDirectoryFallback(t *testing.T) {
	installDir, workingDir := t.TempDir(), t.TempDir()
	key := "SMARTFICO_DOTENV_TEST_FALLBACK"
	unsetEnvForTest(t, key)
	if err := os.WriteFile(filepath.Join(workingDir, ".env"), []byte(key+"=project-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	dir, err := loadDotEnv(filepath.Join(installDir, "smartfico-whatsapp"), workingDir)
	if err != nil || dir != workingDir || os.Getenv(key) != "project-value" {
		t.Fatalf("%s %q %v", dir, os.Getenv(key), err)
	}
}

func TestDotEnvPreservesExistingEnvironment(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SMARTFICO_DOTENV_TEST_EXISTING", "shell-value")
	t.Setenv("SMARTFICO_DOTENV_TEST_EMPTY", "")
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("SMARTFICO_DOTENV_TEST_EXISTING=file-value\nSMARTFICO_DOTENV_TEST_EMPTY=file-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDotEnv(filepath.Join(dir, "smartfico-whatsapp"), dir); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("SMARTFICO_DOTENV_TEST_EXISTING") != "shell-value" || os.Getenv("SMARTFICO_DOTENV_TEST_EMPTY") != "" {
		t.Fatal(".env overrode an existing environment variable")
	}
}

func TestDotEnvOptionalAndMalformed(t *testing.T) {
	dir, workingDir := t.TempDir(), t.TempDir()
	if actual, err := loadDotEnv(filepath.Join(dir, "smartfico-whatsapp"), workingDir); err != nil || actual != workingDir {
		t.Fatal(actual, err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET='sensitive malformed value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := loadDotEnv(filepath.Join(dir, "smartfico-whatsapp"), workingDir)
	if err == nil || strings.Contains(err.Error(), "sensitive") || !strings.Contains(err.Error(), filepath.Join(dir, ".env")) {
		t.Fatalf("unsafe/missing configuration error: %v", err)
	}
}
