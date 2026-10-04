package main

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

var phonePattern = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", a.status)
	mux.HandleFunc("POST /api/messages", a.enqueue)
	mux.HandleFunc("GET /api/messages/{id}", a.getJob)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		if a.token != "" {
			provided := ""
			if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
				provided = strings.TrimPrefix(auth, "Bearer ")
			}
			if subtle.ConstantTimeCompare([]byte(provided), []byte(a.token)) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="WhatsApp"`)
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Authentication required"})
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}

func (a *app) status(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    a.loginStatus,
		"connected": a.client.IsConnected(),
		"logged_in": a.client.IsLoggedIn(),
	})
}

func (a *app) enqueue(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeJSON(w, 415, map[string]string{"error": "Content-Type must be application/json"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	var request struct {
		Number  string `json:"number"`
		Message string `json:"message"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeJSON(w, 400, map[string]string{"error": "Invalid JSON; provide number and message strings"})
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeJSON(w, 400, map[string]string{"error": "Request must contain one JSON object"})
		return
	}
	request.Number = strings.TrimSpace(request.Number)
	if !phonePattern.MatchString(request.Number) {
		writeJSON(w, 400, map[string]string{"error": "number must use international format, e.g. +923205401101"})
		return
	}
	if strings.TrimSpace(request.Message) == "" || !utf8.ValidString(request.Message) || utf8.RuneCountInString(request.Message) > 4096 {
		writeJSON(w, 400, map[string]string{"error": "message must contain 1–4096 characters"})
		return
	}
	if !a.client.IsConnected() || !a.client.IsLoggedIn() {
		writeJSON(w, 503, map[string]string{"error": "WhatsApp is not connected; scan the QR code in the server console"})
		return
	}
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		writeJSON(w, 500, map[string]string{"error": "Could not create job"})
		return
	}
	j := job{ID: hex.EncodeToString(idBytes), Number: request.Number, Message: request.Message, Status: "queued"}
	result, err := a.db.ExecContext(r.Context(), `INSERT INTO jobs(id,number,message,status,created_at)
SELECT ?,?,?,?,? WHERE (SELECT COUNT(*) FROM jobs WHERE status IN ('queued','sending')) < 1000`, j.ID, j.Number, j.Message, j.Status, time.Now().UnixMilli())
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "Could not queue message"})
		return
	}
	count, err := result.RowsAffected()
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "Could not confirm queued message"})
		return
	}
	if count == 0 {
		writeJSON(w, 429, map[string]string{"error": "Message queue is full"})
		return
	}
	w.Header().Set("Location", "/api/messages/"+j.ID)
	writeJSON(w, http.StatusAccepted, j)
}

func (a *app) getJob(w http.ResponseWriter, r *http.Request) {
	var j job
	err := a.db.QueryRowContext(r.Context(), `SELECT id,number,message,status,message_id,error FROM jobs WHERE id=?`, r.PathValue("id")).Scan(&j.ID, &j.Number, &j.Message, &j.Status, &j.MessageID, &j.Error)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, 404, map[string]string{"error": "Message job not found"})
		return
	}
	if err != nil {
		writeJSON(w, 500, map[string]string{"error": "Could not read message job"})
		return
	}
	writeJSON(w, 200, j)
}
