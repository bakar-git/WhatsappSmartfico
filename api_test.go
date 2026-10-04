package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

type fakeTransport struct {
	ready bool
	sent  chan string
}

func (f *fakeTransport) IsConnected() bool { return f.ready }
func (f *fakeTransport) IsLoggedIn() bool  { return f.ready }
func (f *fakeTransport) SendMessage(_ context.Context, to types.JID, msg *waE2E.Message, _ ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	f.sent <- to.String() + ":" + msg.GetConversation()
	return whatsmeow.SendResponse{ID: "test-wa-id"}, nil
}

func testApp(t *testing.T, ready bool) *app {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	if err = initJobs(db); err != nil {
		t.Fatal(err)
	}
	return &app{db: db, client: &fakeTransport{ready: ready, sent: make(chan string, 1)}, loginStatus: "test"}
}

func request(a *app, method, path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	return w
}

func TestValidationAndDisconnected(t *testing.T) {
	a := testApp(t, false)
	for _, body := range []string{
		`{"number":"03205401101","message":"Hi"}`,
		`{"number":"+923205401101","message":" "}`,
		`{"number":"+923205401101","message":"Hi","file":"no"}`,
		`{"number":"+923205401101","message":"Hi"} {}`,
		`{"number":1,"message":"Hi"}`,
		`null`,
	} {
		if w := request(a, "POST", "/api/messages", body, ""); w.Code != 400 {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body)
		}
	}
	w := request(a, "POST", "/api/messages", `{"number":"+923205401101","message":"Hello gorgeous"}`, "")
	if w.Code != 503 {
		t.Fatalf("want disconnected 503, got %d", w.Code)
	}
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("invalid requests queued: %d, %v", count, err)
	}
}

func TestAuthAndConsoleOnly(t *testing.T) {
	a := testApp(t, true)
	a.token = "secret"
	if w := request(a, "GET", "/api/status", "", ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := request(a, "GET", "/api/status", "", "wrong"); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := request(a, "GET", "/api/status", "", "secret"); w.Code != 200 {
		t.Fatal(w.Code)
	}
	for _, path := range []string{"/", "/api/qr"} {
		if w := request(a, "GET", path, "", "secret"); w.Code != 404 {
			t.Fatalf("unexpected browser endpoint %s: %d", path, w.Code)
		}
	}
}

func TestAsyncSend(t *testing.T) {
	a := testApp(t, true)
	w := request(a, "POST", "/api/messages", `{"number":"+923205401101","message":"Hello gorgeous"}`, "")
	if w.Code != 202 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var j job
	if err := json.Unmarshal(w.Body.Bytes(), &j); err != nil {
		t.Fatal(err)
	}
	if j.Status != "queued" || w.Header().Get("Location") != "/api/messages/"+j.ID {
		t.Fatalf("invalid queue response: %+v", j)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); a.work(ctx) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		w = request(a, "GET", "/api/messages/"+j.ID, "", "")
		if err := json.Unmarshal(w.Body.Bytes(), &j); err != nil {
			t.Fatal(err)
		}
		if j.Status == "sent" {
			if j.MessageID != "test-wa-id" {
				t.Fatal(j)
			}
			if got := <-a.client.(*fakeTransport).sent; got != "923205401101@s.whatsapp.net:Hello gorgeous" {
				t.Fatal(got)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("worker never sent: %+v", j)
}

func TestRestartPreservesQueueAndMarksInFlightUnknown(t *testing.T) {
	a := testApp(t, true)
	for _, status := range []string{"queued", "sending", "sent"} {
		if _, err := a.db.Exec(`INSERT INTO jobs(id,number,message,status,created_at) VALUES(?,?,?,?,?)`, status, "+923205401101", "Hi", status, time.Now().UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	if err := initJobs(a.db); err != nil {
		t.Fatal(err)
	}
	for id, expected := range map[string]string{"queued": "queued", "sending": "unknown", "sent": "sent"} {
		var actual string
		if err := a.db.QueryRow(`SELECT status FROM jobs WHERE id=?`, id).Scan(&actual); err != nil || actual != expected {
			t.Fatalf("%s: %s %v", id, actual, err)
		}
	}
}

func TestWarmupLimitsSurviveRestart(t *testing.T) {
	a := testApp(t, true)
	now := time.Now().UnixMilli()
	if _, err := a.db.Exec(`INSERT INTO settings VALUES('warmup_start',?)`, now); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 15; i++ {
		if _, err := a.db.Exec(`INSERT INTO send_attempts VALUES(?,?,?)`, "923205401101@s.whatsapp.net", fmt.Sprint(i), now); err != nil {
			t.Fatal(err)
		}
	}
	w := request(a, "POST", "/api/messages", `{"number":"+923205401101","message":"Receipt #16"}`, "")
	var j job
	if err := json.Unmarshal(w.Body.Bytes(), &j); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); a.work(ctx) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		w = request(a, "GET", "/api/messages/"+j.ID, "", "")
		if err := json.Unmarshal(w.Body.Bytes(), &j); err != nil {
			t.Fatal(err)
		}
		if j.Status == "blocked" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("warm-up not restored: %+v", j)
}

func TestConsoleQR(t *testing.T) {
	var output bytes.Buffer
	if err := printQR(&output, "test-pairing-code"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "\x1b[30;47m") || !strings.ContainsAny(output.String(), "█▀▄") {
		t.Fatal("missing terminal QR modules")
	}
}
