package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

func uploadRequest(t *testing.T, a *app, endpoint, kind, filename string, data []byte, fields map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	for key, value := range map[string]string{"number": "+923205401101", "type": kind, "caption": "Your receipt"} {
		if override, ok := fields[key]; ok {
			value = override
		}
		if err := form.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	for key, value := range fields {
		if key == "number" || key == "type" || key == "caption" {
			continue
		}
		if err := form.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	file, err := form.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write(data); err != nil {
		t.Fatal(err)
	}
	if err = form.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", endpoint, &body)
	r.Header.Set("Content-Type", form.FormDataContentType())
	r.Header.Set("Origin", "https://shop.smartfico.app")
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	return w
}

func testPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 3))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestCORS(t *testing.T) {
	a := testApp(t, true)
	a.token = "secret"
	for _, origin := range []string{"https://shop.smartfico.app", "http://tenant.smartfico.test:5173", "https://branch.shop.smartfico.app"} {
		r := httptest.NewRequest("OPTIONS", "/api/documents", nil)
		r.Header.Set("Origin", origin)
		r.Header.Set("Access-Control-Request-Method", "POST")
		r.Header.Set("Access-Control-Request-Headers", "authorization, content-type")
		r.Header.Set("Access-Control-Request-Private-Network", "true")
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, r)
		if w.Code != 204 || w.Header().Get("Access-Control-Allow-Origin") != origin || w.Header().Get("Access-Control-Allow-Private-Network") != "true" {
			t.Fatalf("%s: %d %+v", origin, w.Code, w.Header())
		}
	}
	for _, origin := range []string{"https://smartfico.app", "https://smartfico.test", "https://evilsmartfico.app", "https://shop.smartfico.app.evil.com", "https://shop.smartfico.app@evil.com", "https://evil.com@shop.smartfico.app", "null", "file://shop.smartfico.app", "https://shop.smartfico.app/path", "https://-bad.smartfico.app"} {
		r := httptest.NewRequest("GET", "/api/status", nil)
		r.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, r)
		if w.Code != 403 || w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("untrusted origin allowed %s: %d", origin, w.Code)
		}
	}
	for _, test := range []struct{ method, headers string }{{"DELETE", ""}, {"POST", "x-unapproved"}} {
		r := httptest.NewRequest("OPTIONS", "/api/documents", nil)
		r.Header.Set("Origin", "https://shop.smartfico.app")
		r.Header.Set("Access-Control-Request-Method", test.method)
		r.Header.Set("Access-Control-Request-Headers", test.headers)
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal(w.Code)
		}
	}
	// Actual cross-origin requests still need the bearer token, and errors
	// carry CORS headers so the Smartfico UI can read them.
	r := httptest.NewRequest("GET", "/api/status", nil)
	r.Header.Set("Origin", "https://shop.smartfico.app")
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	if w.Code != 401 || w.Header().Get("Access-Control-Allow-Origin") != "https://shop.smartfico.app" {
		t.Fatal(w.Code, w.Header())
	}
}

func TestHealth(t *testing.T) {
	a := testApp(t, true)
	if w := request(a, "GET", "/api/health", "", ""); w.Code != 503 {
		t.Fatalf("stopped worker reported ready: %d", w.Code)
	}
	a.setWorkerRunning(true)
	w := request(a, "GET", "/api/health", "", "")
	var health struct {
		Ready      bool `json:"ready"`
		DatabaseOK bool `json:"database_ok"`
		LoggedIn   bool `json:"logged_in"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &health); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || !health.Ready || !health.DatabaseOK || !health.LoggedIn {
		t.Fatal(w.Code, health)
	}
	a.client = &fakeTransport{ready: false}
	if w := request(a, "GET", "/api/health", "", ""); w.Code != 503 {
		t.Fatal(w.Code)
	}
	a.client = &fakeTransport{ready: true}
	a.db.Close()
	if w := request(a, "GET", "/api/health", "", ""); w.Code != 503 {
		t.Fatalf("closed database reported ready: %d", w.Code)
	}
}

func TestMediaValidation(t *testing.T) {
	a := testApp(t, true)
	for _, test := range []struct {
		kind, filename string
		data           []byte
		fields         map[string]string
		code           int
	}{
		{"pdf", "receipt.pdf", []byte("not a PDF"), nil, 415},
		{"image", "receipt.jpg", []byte("%PDF-1.4\n"), nil, 415},
		{"video", "movie.mp4", []byte("data"), nil, 400},
		{"pdf", "receipt.pdf", []byte("%PDF-1.4\n"), map[string]string{"number": "03205401101"}, 400},
		{"pdf", "receipt.pdf", []byte("%PDF-1.4\n"), map[string]string{"caption": strings.Repeat("a", 1025)}, 400},
		{"pdf", "receipt.pdf", []byte("%PDF-1.4\n"), map[string]string{"extra": "bad"}, 400},
		{"pdf", "empty.pdf", nil, nil, 413},
		{"pdf", "big.pdf", bytes.Repeat([]byte("x"), maxFileSize+1), nil, 413},
	} {
		w := uploadRequest(t, a, "/api/documents", test.kind, test.filename, test.data, test.fields)
		if w.Code != test.code {
			t.Fatalf("%s %s: %d %s", test.kind, test.filename, w.Code, w.Body)
		}
	}
	if w := request(a, "POST", "/api/documents", `{}`, ""); w.Code != http.StatusUnsupportedMediaType {
		t.Fatal(w.Code)
	}
	a.client = &fakeTransport{ready: false}
	if w := uploadRequest(t, a, "/api/documents", "pdf", "receipt.pdf", []byte("%PDF-1.4\n"), nil); w.Code != 503 {
		t.Fatal(w.Code)
	}
	var count int
	if err := a.db.QueryRow(`SELECT count(*) FROM jobs`).Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}

func TestMediaPersistsAndQueueIsAtomic(t *testing.T) {
	a := testApp(t, true)
	data := []byte("%PDF-1.4\nreceipt\n%%EOF")
	w := uploadRequest(t, a, "/api/messages", "pdf", `..\..\receipt.exe`, data, nil)
	var j job
	if err := json.Unmarshal(w.Body.Bytes(), &j); err != nil {
		t.Fatal(err)
	}
	if w.Code != 202 || j.Filename != "receipt.pdf" || j.Type != "pdf" || j.MimeType != "application/pdf" || j.Size != int64(len(data)) {
		t.Fatal(w.Code, w.Body)
	}
	if err := initJobs(a.db); err != nil {
		t.Fatal(err)
	}
	var stored []byte
	if err := a.db.QueryRow(`SELECT data FROM job_media WHERE job_id=?`, j.ID).Scan(&stored); err != nil || !bytes.Equal(stored, data) {
		t.Fatal(err)
	}
	w = request(a, "GET", "/api/messages/"+j.ID, "", "")
	if !strings.Contains(w.Body.String(), `"type":"pdf"`) || !strings.Contains(w.Body.String(), `"status":"queued"`) {
		t.Fatal(w.Body)
	}
	// A failed attachment insert must roll back the job, too.
	if _, err := a.db.Exec(`CREATE TRIGGER fail_attachment BEFORE INSERT ON job_media BEGIN SELECT RAISE(ABORT,'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	w = uploadRequest(t, a, "/api/documents", "pdf", "receipt.pdf", data, nil)
	if w.Code != 500 {
		t.Fatal(w.Code)
	}
	var count int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
}

type uploadCall struct {
	data []byte
	kind whatsmeow.MediaType
}
type mediaTransport struct {
	fakeTransport
	uploads   chan uploadCall
	messages  chan *waE2E.Message
	uploadErr error
}

func (f *mediaTransport) Upload(ctx context.Context, data []byte, kind whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	f.uploads <- uploadCall{data, kind}
	if f.uploadErr != nil {
		return whatsmeow.UploadResponse{}, f.uploadErr
	}
	return f.fakeTransport.Upload(ctx, data, kind)
}
func (f *mediaTransport) SendMessage(_ context.Context, _ types.JID, msg *waE2E.Message, _ ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	f.messages <- msg
	return whatsmeow.SendResponse{ID: "test-media-id"}, nil
}

func TestMediaWorker(t *testing.T) {
	for _, kind := range []string{"pdf", "image", "jpeg", "upload_failure"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			a := testApp(t, true)
			transport := &mediaTransport{fakeTransport: fakeTransport{ready: true}, uploads: make(chan uploadCall, 1), messages: make(chan *waE2E.Message, 1)}
			a.client = transport
			data := []byte("%PDF-1.4\nreceipt\n%%EOF")
			filename := "receipt.pdf"
			requestType := kind
			if kind == "image" {
				data = testPNG(t)
				filename = "receipt.png"
			}
			if kind == "jpeg" {
				var buf bytes.Buffer
				if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 3)), nil); err != nil {
					t.Fatal(err)
				}
				data, filename, requestType = buf.Bytes(), "receipt.jpeg", "image"
			}
			if kind == "upload_failure" {
				requestType = "pdf"
				transport.uploadErr = errors.New("upload unavailable")
			}
			w := uploadRequest(t, a, "/api/documents", requestType, filename, data, nil)
			if w.Code != 202 {
				t.Fatal(w.Code, w.Body)
			}
			var j job
			if err := json.Unmarshal(w.Body.Bytes(), &j); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); a.work(ctx) }()
			defer func() { cancel(); <-done }()
			terminal := "sent"
			if kind == "upload_failure" {
				terminal = "failed"
			}
			deadline := time.Now().Add(15 * time.Second)
			for time.Now().Before(deadline) {
				w = request(a, "GET", "/api/messages/"+j.ID, "", "")
				if err := json.Unmarshal(w.Body.Bytes(), &j); err != nil {
					t.Fatal(err)
				}
				if j.Status == terminal {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if j.Status != terminal {
				t.Fatalf("unexpected job status: %+v", j)
			}
			call := <-transport.uploads
			if !bytes.Equal(call.data, data) {
				t.Fatal("upload changed file contents")
			}
			if kind == "upload_failure" {
				if len(transport.messages) != 0 {
					t.Fatal("sent a message after upload failure")
				}
			} else {
				msg := <-transport.messages
				if kind == "pdf" {
					doc := msg.GetDocumentMessage()
					if call.kind != whatsmeow.MediaDocument || doc == nil || doc.GetMimetype() != "application/pdf" || doc.GetFileName() != "receipt.pdf" || doc.GetCaption() != "Your receipt" || doc.GetFileLength() != uint64(len(data)) || doc.GetURL() == "" || len(doc.GetMediaKey()) == 0 || len(doc.GetFileSHA256()) == 0 || len(doc.GetFileEncSHA256()) == 0 {
						t.Fatal("invalid document payload", msg)
					}
				} else {
					img := msg.GetImageMessage()
					mimeType := "image/png"
					if kind == "jpeg" {
						mimeType = "image/jpeg"
					}
					if call.kind != whatsmeow.MediaImage || img == nil || img.GetMimetype() != mimeType || img.GetCaption() != "Your receipt" || img.GetWidth() != 2 || img.GetHeight() != 3 || img.GetFileLength() != uint64(len(data)) || img.GetDirectPath() == "" {
						t.Fatal("invalid image payload", msg)
					}
				}
			}
			var remaining int
			if err := a.db.QueryRow(`SELECT length(data) FROM job_media WHERE job_id=?`, j.ID).Scan(&remaining); err != nil || remaining != 0 {
				t.Fatal("completed job retained upload bytes", remaining, err)
			}
		})
	}
}
