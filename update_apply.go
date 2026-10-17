package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/gofrs/flock"
)

type updatePlan struct {
	Target       string
	Staged       string
	StateDir     string
	WorkingDir   string
	Version      string
	Address      string
	Token        string
	BinarySHA256 string
}

func tryUpdateLock(dir string) (*flock.Flock, bool, error) {
	lock := flock.New(filepath.Join(dir, "update.lock"))
	locked, err := lock.TryLock()
	if err != nil || !locked {
		_ = lock.Close()
	}
	return lock, locked, err
}

func copyExecutable(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0700)
	if err != nil {
		return err
	}
	_, err = io.Copy(output, input)
	if err == nil {
		err = output.Sync()
	}
	closeErr := output.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func executableHash(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err = io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func startUpdateHelper(ctx context.Context, plan updatePlan) error {
	var err error
	plan.BinarySHA256, err = executableHash(plan.Staged)
	if err != nil {
		return err
	}
	helper := filepath.Join(plan.StateDir, "updater")
	if runtime.GOOS == "windows" {
		helper += ".exe"
	}
	if err := copyExecutable(plan.Target, helper); err != nil {
		return fmt.Errorf("prepare update helper: %w", err)
	}
	planPath := filepath.Join(plan.StateDir, "update-plan.json")
	data, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	if err := os.WriteFile(planPath, data, 0600); err != nil {
		return err
	}
	ready := planPath + ".ready"
	_ = os.Remove(ready)
	logFile, err := os.OpenFile(filepath.Join(plan.StateDir, "service.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	cmd := exec.Command(helper, "--apply-update", planPath)
	cmd.Dir = plan.WorkingDir
	cmd.Stdout, cmd.Stderr = logFile, logFile
	configureDetached(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(ready); err == nil {
			return nil
		}
		select {
		case <-done:
			return errors.New("update helper exited before becoming ready")
		default:
		}
		if !wait(ctx, 50*time.Millisecond) {
			_ = cmd.Process.Kill()
			return ctx.Err()
		}
	}
	_ = cmd.Process.Kill()
	return errors.New("update helper did not become ready; keeping current service")
}

func runUpdateHelper(planPath string) error {
	data, err := os.ReadFile(planPath)
	if err != nil {
		return err
	}
	var plan updatePlan
	if err := json.Unmarshal(data, &plan); err != nil {
		return err
	}
	if !filepath.IsAbs(plan.Target) || !filepath.IsAbs(plan.Staged) || filepath.Dir(plan.Target) != filepath.Dir(plan.Staged) || filepath.Base(plan.Target) == filepath.Base(plan.Staged) {
		return errors.New("invalid update target/staging paths")
	}
	guard, locked, err := tryUpdateLock(plan.StateDir)
	if err != nil {
		return err
	}
	if !locked {
		return errors.New("another update helper is active")
	}
	defer guard.Close()
	defer os.Remove(planPath)
	defer os.Remove(planPath + ".ready")
	defer os.Remove(plan.Staged)
	if hash, err := executableHash(plan.Staged); err != nil || hash != plan.BinarySHA256 {
		return errors.New("staged executable changed before installation")
	}
	if err := os.WriteFile(planPath+".ready", []byte("ready"), 0600); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var lock *flock.Flock
	locked = false
	for ctx.Err() == nil {
		lock, locked, err = tryInstanceLock(plan.StateDir)
		if err != nil {
			return err
		}
		if locked {
			break
		}
		if !wait(ctx, 100*time.Millisecond) {
			return errors.New("old service did not stop; update cancelled")
		}
	}
	if !locked {
		return errors.New("old service did not stop; update cancelled")
	}
	defer lock.Close()
	// On Windows the old executable can briefly remain locked while the process
	// exits. Retry the rename while holding the instance lock, before replacement.
	backup := plan.Target + ".previous"
	_ = os.Remove(backup)
	for {
		err = os.Rename(plan.Target, backup)
		if err == nil {
			break
		}
		if !wait(ctx, 100*time.Millisecond) {
			_ = lock.Close()
			_, _ = restartUpdatedService(plan)
			return fmt.Errorf("cannot back up installed executable: %w", err)
		}
	}
	if err := os.Rename(plan.Staged, plan.Target); err != nil {
		_ = os.Rename(backup, plan.Target)
		_ = lock.Close()
		_, _ = restartUpdatedService(plan)
		return fmt.Errorf("install update: %w", err)
	}
	_ = lock.Close()
	process, err := restartUpdatedService(plan)
	if err == nil {
		err = waitForUpdatedService(plan, process, 30*time.Second)
	}
	if err == nil {
		_ = os.Remove(filepath.Join(plan.StateDir, "failed-update-version"))
		fmt.Printf("Update to %s installed successfully\n", plan.Version)
		return nil
	}
	if process != nil {
		_ = process.Process.Kill()
		_ = process.Wait()
	}
	// Roll back only after the failed new process has exited. Preserve .env,
	// session DB, queued receipts, and the old executable throughout the update.
	if removeErr := os.Remove(plan.Target); removeErr != nil {
		return fmt.Errorf("new service failed and rollback could not remove it: %w", removeErr)
	}
	if restoreErr := os.Rename(backup, plan.Target); restoreErr != nil {
		return fmt.Errorf("new service failed and rollback failed: %w", restoreErr)
	}
	_ = os.WriteFile(filepath.Join(plan.StateDir, "failed-update-version"), []byte(normalizeVersion(plan.Version)), 0600)
	_, restartErr := restartUpdatedService(plan)
	if restartErr != nil {
		return fmt.Errorf("restored previous binary but restart failed: %w", restartErr)
	}
	return fmt.Errorf("update startup failed; restored the previous binary: %w", err)
}

func restartUpdatedService(plan updatePlan) (*exec.Cmd, error) {
	logFile, err := os.OpenFile(filepath.Join(plan.StateDir, "service.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	defer logFile.Close()
	cmd := exec.Command(plan.Target, backgroundChildArgument)
	cmd.Dir = plan.WorkingDir
	cmd.Stdout, cmd.Stderr = logFile, logFile
	configureDetached(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

func waitForUpdatedService(plan updatePlan, process *exec.Cmd, timeout time.Duration) error {
	host, port, err := net.SplitHostPort(plan.Address)
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	endpoint := "http://" + net.JoinHostPort(host, port) + "/api/health"
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	for ctx.Err() == nil {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if plan.Token != "" {
			req.Header.Set("Authorization", "Bearer "+plan.Token)
		}
		response, err := client.Do(req)
		if err == nil {
			var state struct {
				Version       string `json:"version"`
				DatabaseOK    bool   `json:"database_ok"`
				WorkerRunning bool   `json:"worker_running"`
			}
			decodeErr := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&state)
			response.Body.Close()
			if decodeErr == nil && state.Version == plan.Version && state.DatabaseOK && state.WorkerRunning {
				_ = process.Process.Release()
				return nil
			}
		}
		if !wait(ctx, 200*time.Millisecond) {
			break
		}
	}
	return errors.New("updated service did not report healthy startup")
}
