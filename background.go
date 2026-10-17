package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gofrs/flock"
)

const backgroundChildArgument = "--background-service"

func instanceDirectory() (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(cache, "smartfico-whatsapp")
	return dir, os.MkdirAll(dir, 0700)
}

// Keep the lock file on disk: deleting it could let another process lock a
// different inode while the old service still owns the original lock.
func tryInstanceLock(dir string) (*flock.Flock, bool, error) {
	lock := flock.New(filepath.Join(dir, "instance.lock"))
	locked, err := lock.TryLock()
	if err != nil || !locked {
		_ = lock.Close()
	}
	return lock, locked, err
}

func backgroundMode(args []string) (enabled, child bool, err error) {
	enabled, err = strconv.ParseBool(env("BACKGROUND_ON_CLOSE", "false"))
	if err != nil {
		return false, false, errors.New("BACKGROUND_ON_CLOSE must be true or false")
	}
	for _, arg := range args {
		if arg == "--startup" {
			continue
		}
		if arg != backgroundChildArgument {
			return false, false, fmt.Errorf("unknown argument: %s", arg)
		}
		child = true
	}
	return enabled, child, nil
}

func announceExisting(dir string) {
	pid, _ := os.ReadFile(filepath.Join(dir, "service.pid"))
	fmt.Printf("Smartfico WhatsApp is already running (PID %s).\n", strings.TrimSpace(string(pid)))
	fmt.Printf("Background log, if enabled: %s\n", filepath.Join(dir, "service.log"))
}

func launchBackground(executable, workingDir, stateDir string, follow bool) error {
	lock, locked, err := tryInstanceLock(stateDir)
	if err != nil {
		return err
	}
	if !locked {
		announceExisting(stateDir)
		return nil
	}
	if err := lock.Close(); err != nil {
		return err
	}
	logPath := filepath.Join(stateDir, "service.log")
	output, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer output.Close()
	info, err := output.Stat()
	if err != nil {
		return err
	}
	cmd := exec.Command(executable, backgroundChildArgument)
	cmd.Dir = workingDir
	cmd.Stdout, cmd.Stderr = output, output
	cmd.Env = os.Environ()
	configureDetached(cmd)
	if err = cmd.Start(); err != nil {
		return fmt.Errorf("start background service: %w", err)
	}
	if !follow {
		return cmd.Process.Release()
	}
	// The child writes to a file, never to the launcher's terminal or pipes, so
	// losing this console cannot interrupt a send or the next pairing QR.
	fmt.Printf("Background service started (PID %d). Close this console or press Ctrl+C to leave it running.\n", cmd.Process.Pid)
	fmt.Printf("Log: %s\nStop the service through your OS process manager using that PID.\n", logPath)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return followServiceLog(ctx, logPath, info.Size(), os.Stdout, done)
}

func followServiceLog(ctx context.Context, path string, offset int64, output io.Writer, done <-chan error) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err = file.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := io.Copy(output, file); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case err := <-done:
			_, _ = io.Copy(output, file)
			return err
		case <-ticker.C:
		}
	}
}
