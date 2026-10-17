package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

func checksum(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }

func updateZIP(t *testing.T, entries map[string][]byte) []byte {
	t.Helper()
	var result bytes.Buffer
	archive := zip.NewWriter(&result)
	for name, content := range entries {
		entry, err := archive.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return result.Bytes()
}

func testUpdater(t *testing.T, handler http.Handler) (*updater, *httptest.Server) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	dir := t.TempDir()
	executable := filepath.Join(dir, "installed")
	if err := os.WriteFile(executable, []byte("original binary"), 0755); err != nil {
		t.Fatal(err)
	}
	return &updater{client: server.Client(), manifestURL: server.URL + "/.release", executable: executable, stateDir: t.TempDir(), configDir: dir}, server
}

func TestReleaseManifestVersionPlatformAndChecksum(t *testing.T) {
	var manifest releaseManifest
	u, server := testUpdater(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(manifest) }))
	platform := runtime.GOOS + "-" + runtime.GOARCH
	manifest = releaseManifest{Version: "99.0.0", Assets: map[string]releaseAsset{platform: {Link: server.URL + "/app.zip", SHA256: strings.Repeat("a", 64)}}}
	if _, asset, available, err := u.latest(context.Background()); err != nil || !available || asset.Link != server.URL+"/app.zip" {
		t.Fatal(available, err, asset)
	}
	manifest.Version = version
	if _, _, available, err := u.latest(context.Background()); err != nil || available {
		t.Fatal("current release should not install", available, err)
	}
	manifest.Version = "0.0.1"
	if _, _, available, err := u.latest(context.Background()); err != nil || available {
		t.Fatal("downgrade allowed", available, err)
	}
	manifest.Version = "99.0.0"
	manifest.Assets = map[string]releaseAsset{"unsupported": {}}
	if _, _, available, err := u.latest(context.Background()); err != nil || available {
		t.Fatal("wrong platform offered", available, err)
	}
	manifest.Assets = nil
	manifest.releaseAsset = releaseAsset{Link: server.URL + "/app.exe", SHA256: "bad"}
	if _, _, _, err := u.latest(context.Background()); err == nil {
		t.Fatal("missing checksum accepted")
	}
	manifest.SHA256 = strings.Repeat("a", 64)
	if err := os.WriteFile(filepath.Join(u.stateDir, "failed-update-version"), []byte("v99.0.0"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, available, err := u.latest(context.Background()); err != nil || available {
		t.Fatal("failed version was offered again", available, err)
	}
}

func TestStagedZIPIgnoresConfigAndTraversal(t *testing.T) {
	name := "smartfico-whatsapp"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	data := updateZIP(t, map[string][]byte{name: []byte("new executable"), ".env": []byte("API_TOKEN=attacker"), "../../outside": []byte("bad")})
	u, server := testUpdater(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(data) }))
	config := filepath.Join(filepath.Dir(u.executable), ".env")
	if err := os.WriteFile(config, []byte("API_TOKEN=original"), 0600); err != nil {
		t.Fatal(err)
	}
	staged, err := u.stage(context.Background(), releaseAsset{Link: server.URL + "/app.zip", SHA256: checksum(data)})
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(staged)
	content, err := os.ReadFile(staged)
	if err != nil || string(content) != "new executable" {
		t.Fatal(string(content), err)
	}
	original, _ := os.ReadFile(u.executable)
	settings, _ := os.ReadFile(config)
	if string(original) != "original binary" || string(settings) != "API_TOKEN=original" {
		t.Fatal("staging overwrote installed files")
	}
	if filepath.Dir(staged) != filepath.Dir(u.executable) {
		t.Fatal("staged binary must be on the target filesystem")
	}
}

func TestUpdateChecksumAndZIPFailuresLeaveOriginal(t *testing.T) {
	data := []byte("untrusted contents")
	u, server := testUpdater(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(data) }))
	for _, asset := range []releaseAsset{{Link: server.URL + "/app.exe", SHA256: strings.Repeat("0", 64)}, {Link: server.URL + "/app.zip", SHA256: checksum(data)}} {
		if _, err := u.stage(context.Background(), asset); err == nil {
			t.Fatal("invalid update staged")
		}
	}
	files, err := os.ReadDir(filepath.Dir(u.executable))
	if err != nil || len(files) != 1 {
		t.Fatal("failed download left staging files", files, err)
	}
	content, _ := os.ReadFile(u.executable)
	if string(content) != "original binary" {
		t.Fatal("original binary changed")
	}
}

func TestUpdateURLsAndConfiguration(t *testing.T) {
	for _, raw := range []string{"http://github.com/app", "file:///app", "https://user:secret@github.com/app", "https://github.com/app#fragment"} {
		if err := validateUpdateURL(raw); err == nil {
			t.Fatal("unsafe URL accepted", raw)
		}
	}
	t.Setenv("UPDATE_ENABLED", "true")
	t.Setenv("UPDATE_CHECK_INTERVAL", "5s")
	if _, err := newUpdater("/opt/app", "/tmp/state", "/opt", "127.0.0.1:8080", ""); err == nil {
		t.Fatal("unbounded update polling accepted")
	}
	t.Setenv("UPDATE_ENABLED", "false")
	if u, err := newUpdater("/opt/app", "/tmp/state", "/opt", "127.0.0.1:8080", ""); err != nil || u != nil {
		t.Fatal(u, err)
	}
}

func TestUpdateMaintenanceRejectsRequests(t *testing.T) {
	a := testApp(t, true)
	a.updating.Store(true)
	w := request(a, "POST", "/api/messages", `{"number":"+923205401101","message":"Hi"}`, "")
	if w.Code != 503 {
		t.Fatal(w.Code, w.Body)
	}
	w = request(a, "GET", "/api/health", "", "")
	if w.Code != 503 || !strings.Contains(w.Body.String(), `"updating":true`) {
		t.Fatal(w.Code, w.Body)
	}
}

type blockedUpdateSender struct {
	fakeTransport
	entered chan struct{}
	resume  chan struct{}
}

func (f *blockedUpdateSender) SendMessage(ctx context.Context, to types.JID, msg *waE2E.Message, extra ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	close(f.entered)
	select {
	case <-ctx.Done():
		return whatsmeow.SendResponse{}, ctx.Err()
	case <-f.resume:
		return f.fakeTransport.SendMessage(ctx, to, msg, extra...)
	}
}

func TestWorkerDrainsActiveSendAndPreservesPendingJobForUpdate(t *testing.T) {
	t.Parallel()
	a := testApp(t, true)
	sender := &blockedUpdateSender{fakeTransport: fakeTransport{ready: true, sent: make(chan string, 1)}, entered: make(chan struct{}), resume: make(chan struct{})}
	a.client = sender
	var jobs [2]job
	for index := range jobs {
		response := request(a, "POST", "/api/messages", `{"number":"+923205401101","message":"Receipt"}`, "")
		if response.Code != 202 {
			t.Fatal(response.Code, response.Body)
		}
		if err := json.Unmarshal(response.Body.Bytes(), &jobs[index]); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); a.work(ctx) }()
	defer func() { cancel(); <-workerDone }()
	select {
	case <-sender.entered:
	case <-time.After(15 * time.Second):
		t.Fatal("worker did not start send")
	}
	a.updating.Store(true)
	drained := make(chan struct{})
	go func() { a.sendMu.Lock(); a.sendMu.Unlock(); close(drained) }()
	select {
	case <-drained:
		t.Fatal("worker released update guard during active send")
	case <-time.After(20 * time.Millisecond):
	}
	close(sender.resume)
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("update did not resume after send completed")
	}
	first, err := readJob(a.db.QueryRow(jobSelect+` WHERE j.id=?`, jobs[0].ID))
	if err != nil || first.Status != "sent" {
		t.Fatal("active send was interrupted", first, err)
	}
	second, err := readJob(a.db.QueryRow(jobSelect+` WHERE j.id=?`, jobs[1].ID))
	if err != nil || second.Status != "queued" {
		t.Fatal("pending job was not preserved", second, err)
	}
}
