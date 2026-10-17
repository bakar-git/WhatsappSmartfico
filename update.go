package main

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

// Override when building a release: -ldflags="-X main.version=0.3.0".
var version = "0.3.0"

const defaultManifestURL = "https://github.com/bakar-git/WhatsappSmartfico/releases/latest/download/default.release"
const maxUpdateSize = 128 << 20

type releaseAsset struct {
	Link   string `json:"link"`
	SHA256 string `json:"sha256"`
}

type releaseManifest struct {
	Version string `json:"version"`
	releaseAsset
	Assets map[string]releaseAsset `json:"assets,omitempty"`
}

type updater struct {
	client      *http.Client
	manifestURL string
	interval    time.Duration
	executable  string
	stateDir    string
	configDir   string
	address     string
	token       string
}

func normalizeVersion(value string) string {
	return "v" + strings.TrimPrefix(strings.TrimSpace(value), "v")
}

func newUpdater(executable, stateDir, configDir, address, token string) (*updater, error) {
	enabled, err := boolSetting("UPDATE_ENABLED", true)
	if err != nil {
		return nil, err
	}
	if !enabled || temporaryExecutable(executable) {
		return nil, nil
	}
	interval, err := time.ParseDuration(env("UPDATE_CHECK_INTERVAL", "6h"))
	if err != nil || interval < time.Minute {
		return nil, errors.New("UPDATE_CHECK_INTERVAL must be a duration of at least 1m")
	}
	manifestURL := env("UPDATE_MANIFEST_URL", defaultManifestURL)
	if err := validateUpdateURL(manifestURL); err != nil {
		return nil, fmt.Errorf("UPDATE_MANIFEST_URL: %w", err)
	}
	return &updater{
		client: &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("too many update redirects")
			}
			return validateUpdateURL(req.URL.String())
		}},
		manifestURL: manifestURL, interval: interval, executable: executable, stateDir: stateDir, configDir: configDir, address: address, token: token,
	}, nil
}

func validateUpdateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return errors.New("update URLs must use HTTPS without embedded credentials or fragments")
	}
	return nil
}

func (u *updater) get(ctx context.Context, link string) (*http.Response, error) {
	if err := validateUpdateURL(link); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "SmartficoWhatsapp/"+version)
	req.Header.Set("Cache-Control", "no-cache")
	response, err := u.client.Do(req)
	if err != nil {
		return nil, errors.New("update request failed; check connection, TLS, and configured URL")
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("update server returned HTTP %d", response.StatusCode)
	}
	return response, nil
}

func (u *updater) latest(ctx context.Context) (releaseManifest, releaseAsset, bool, error) {
	var manifest releaseManifest
	response, err := u.get(ctx, u.manifestURL)
	if err != nil {
		return manifest, releaseAsset{}, false, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return manifest, releaseAsset{}, false, errors.New("release manifest is unreadable or exceeds 1 MiB")
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return manifest, releaseAsset{}, false, errors.New("invalid .release JSON")
	}
	remote, current := normalizeVersion(manifest.Version), normalizeVersion(version)
	if !semver.IsValid(remote) || !semver.IsValid(current) {
		return manifest, releaseAsset{}, false, errors.New("release and binary versions must be semantic versions")
	}
	if semver.Compare(remote, current) <= 0 {
		return manifest, releaseAsset{}, false, nil
	}
	if failed, _ := os.ReadFile(filepath.Join(u.stateDir, "failed-update-version")); strings.TrimSpace(string(failed)) == remote {
		return manifest, releaseAsset{}, false, nil
	}
	asset := manifest.releaseAsset
	if len(manifest.Assets) > 0 {
		var ok bool
		asset, ok = manifest.Assets[runtime.GOOS+"-"+runtime.GOARCH]
		if !ok {
			return manifest, asset, false, nil
		}
	}
	if err := validateUpdateURL(asset.Link); err != nil {
		return manifest, asset, false, err
	}
	hash, err := hex.DecodeString(asset.SHA256)
	if err != nil || len(hash) != sha256.Size {
		return manifest, asset, false, errors.New("release asset requires its SHA-256 checksum")
	}
	return manifest, asset, true, nil
}

func (u *updater) stage(ctx context.Context, asset releaseAsset) (staged string, resultErr error) {
	response, err := u.get(ctx, asset.Link)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	dir := filepath.Dir(u.executable)
	download, err := os.CreateTemp(dir, ".smartfico-download-*")
	if err != nil {
		return "", fmt.Errorf("installation folder must be writable: %w", err)
	}
	defer os.Remove(download.Name())
	defer download.Close()
	hash := sha256.New()
	count, err := io.Copy(io.MultiWriter(download, hash), io.LimitReader(response.Body, maxUpdateSize+1))
	if err != nil || count == 0 || count > maxUpdateSize {
		return "", errors.New("update download failed, is empty, or exceeds 128 MiB")
	}
	if !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), asset.SHA256) {
		return "", errors.New("update checksum mismatch; keeping the installed binary")
	}
	if err = download.Close(); err != nil {
		return "", err
	}
	// ZIPs may contain .env or other files, but only the executable is read.
	// No archive paths are extracted onto the filesystem.
	link, _ := url.Parse(asset.Link)
	var source io.ReadCloser
	if strings.HasSuffix(strings.ToLower(link.Path), ".zip") {
		archive, err := zip.OpenReader(download.Name())
		if err != nil {
			return "", errors.New("invalid update ZIP")
		}
		defer archive.Close()
		name := "smartfico-whatsapp"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		var selected *zip.File
		for _, entry := range archive.File {
			if path.Base(entry.Name) != name {
				continue
			}
			if selected != nil || !entry.Mode().IsRegular() || entry.UncompressedSize64 > maxUpdateSize {
				return "", errors.New("update ZIP must contain exactly one regular executable of at most 128 MiB")
			}
			selected = entry
		}
		if selected == nil {
			return "", errors.New("update ZIP does not contain smartfico-whatsapp for this OS")
		}
		source, err = selected.Open()
		if err != nil {
			return "", err
		}
	} else {
		source, err = os.Open(download.Name())
		if err != nil {
			return "", err
		}
	}
	defer source.Close()
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	file, err := os.CreateTemp(dir, ".smartfico-update-*"+suffix)
	if err != nil {
		return "", err
	}
	staged = file.Name()
	defer func() {
		if resultErr != nil {
			_ = os.Remove(staged)
		}
	}()
	count, err = io.Copy(file, io.LimitReader(source, maxUpdateSize+1))
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil || count == 0 || count > maxUpdateSize {
		return staged, errors.New("could not stage update executable")
	}
	info, err := os.Stat(u.executable)
	if err != nil {
		return staged, err
	}
	if err = os.Chmod(staged, info.Mode().Perm()|0700); err != nil {
		return staged, err
	}
	return staged, nil
}

func (a *app) watchUpdates(ctx context.Context, u *updater, restart chan<- struct{}) {
	if u == nil {
		return
	}
	for ctx.Err() == nil {
		if err := a.checkUpdate(ctx, u, restart); err != nil && ctx.Err() == nil {
			log.Printf("Update check: %v", err)
		}
		if a.updating.Load() || !wait(ctx, u.interval) {
			return
		}
	}
}

func (a *app) checkUpdate(ctx context.Context, u *updater, restart chan<- struct{}) error {
	// A replacement may still be validating startup/rolling back. Do not
	// overwrite the active helper or its plan during that window.
	guard, locked, err := tryUpdateLock(u.stateDir)
	if err != nil || !locked {
		return err
	}
	_ = guard.Close()
	manifest, asset, available, err := u.latest(ctx)
	if err != nil || !available {
		return err
	}
	log.Printf("Downloading update %s", manifest.Version)
	staged, err := u.stage(ctx, asset)
	if err != nil {
		return err
	}
	ownedByHelper := false
	defer func() {
		if !ownedByHelper {
			_ = os.Remove(staged)
		}
	}()
	verifyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	command := exec.CommandContext(verifyCtx, staged, "--version")
	command.Dir = u.configDir
	output, err := command.Output()
	cancel()
	if err != nil || strings.TrimSpace(string(output)) != strings.TrimPrefix(normalizeVersion(manifest.Version), "v") {
		return errors.New("downloaded binary failed its version/platform check")
	}
	a.updating.Store(true)
	// Wait for the current send to finish. While draining, new jobs get 503
	// and queued jobs remain in SQLite for the replacement process.
	a.sendMu.Lock()
	defer a.sendMu.Unlock()
	if ctx.Err() != nil {
		a.updating.Store(false)
		return ctx.Err()
	}
	plan := updatePlan{Target: u.executable, Staged: staged, StateDir: u.stateDir, WorkingDir: u.configDir, Version: strings.TrimPrefix(normalizeVersion(manifest.Version), "v"), Address: u.address, Token: u.token}
	if err := startUpdateHelper(ctx, plan); err != nil {
		a.updating.Store(false)
		return err
	}
	ownedByHelper = true
	log.Printf("Restarting to install version %s", manifest.Version)
	restart <- struct{}{}
	return nil
}
