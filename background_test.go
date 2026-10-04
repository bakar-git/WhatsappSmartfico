package main

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInstanceLockRejectsSecondOwner(t *testing.T) {
	dir := t.TempDir()
	first, locked, err := tryInstanceLock(dir)
	if err != nil || !locked {
		t.Fatal(locked, err)
	}
	t.Cleanup(func() { first.Close() })
	if _, locked, err := tryInstanceLock(dir); err != nil || locked {
		t.Fatal("second owner acquired lock", locked, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	next, locked, err := tryInstanceLock(dir)
	if err != nil || !locked {
		t.Fatal("released lock unavailable", locked, err)
	}
	next.Close()
}

func TestInstanceLockHelper(t *testing.T) {
	dir := os.Getenv("SMARTFICO_LOCK_TEST_DIR")
	if dir == "" {
		return
	}
	lock, locked, err := tryInstanceLock(dir)
	if err != nil || !locked {
		t.Fatal(locked, err)
	}
	defer lock.Close()
	if _, err := os.Stdout.WriteString("LOCKED\n"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Second)
}

func TestInstanceLockReleasedAfterProcessDeath(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestInstanceLockHelper$")
	cmd.Env = append(os.Environ(), "SMARTFICO_LOCK_TEST_DIR="+dir)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() || scanner.Text() != "LOCKED" {
		_ = cmd.Wait()
		t.Fatal("helper did not acquire instance lock")
	}
	if _, locked, err := tryInstanceLock(dir); err != nil || locked {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("other process's lock was not enforced", locked, err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	lock, locked, err := tryInstanceLock(dir)
	if err != nil || !locked {
		t.Fatal("process death left a stale lock", locked, err)
	}
	lock.Close()
}

func TestBackgroundMode(t *testing.T) {
	t.Setenv("BACKGROUND_ON_CLOSE", "true")
	if enabled, child, err := backgroundMode(nil); err != nil || !enabled || child {
		t.Fatal(enabled, child, err)
	}
	if enabled, child, err := backgroundMode([]string{backgroundChildArgument}); err != nil || !enabled || !child {
		t.Fatal(enabled, child, err)
	}
	t.Setenv("BACKGROUND_ON_CLOSE", "false")
	if enabled, child, err := backgroundMode(nil); err != nil || enabled || child {
		t.Fatal(enabled, child, err)
	}
	t.Setenv("BACKGROUND_ON_CLOSE", "invalid")
	if _, _, err := backgroundMode(nil); err == nil {
		t.Fatal("invalid setting was accepted")
	}
}

func TestLogViewerReadsFinalOutputAndCanClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "service.log")
	if err := os.WriteFile(path, []byte("previous\nnew QR\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	done := make(chan error, 1)
	done <- nil
	if err := followServiceLog(context.Background(), path, int64(len("previous\n")), &output, done); err != nil {
		t.Fatal(err)
	}
	if output.String() != "new QR\n" {
		t.Fatal(output.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := followServiceLog(ctx, path, 0, &output, nil); err != nil {
		t.Fatal(err)
	}
}
