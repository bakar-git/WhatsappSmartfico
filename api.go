package main

import (
	"context"
	"crypto/subtle"
	"database/sql"
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
	mux.HandleFunc("GET /api/health", a.health)
	mux.HandleFunc("POST /api/messages", a.enqueue)
	mux.HandleFunc("POST /api/message", a.enqueue)
	mux.HandleFunc("POST /api/documents", a.enqueueMedia)
	mux.HandleFunc("GET /api/messages/{id}", a.getJob)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		if !cors(w, r) {
			return
		}
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
		if r.Method == http.MethodPost && a.updating.Load() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Installing an update; retry after the service restarts"})
			return
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
		"version":   version,
		"updating":  a.updating.Load(),
		"status":    a.loginStatus,
		"connected": a.client.IsConnected(),
		"logged_in": a.client.IsLoggedIn(),
	})
}

func (a *app) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	databaseOK := a.db.PingContext(ctx) == nil
	connected, loggedIn := a.client.IsConnected(), a.client.IsLoggedIn()
	a.mu.RLock()
	workerRunning, status := a.workerRunning, a.loginStatus
	a.mu.RUnlock()
	ready := databaseOK && connected && loggedIn && workerRunning && !a.updating.Load()
	code := http.StatusOK
	if !ready {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{
		"service": "smartfico-whatsapp", "api_running": true, "ready": ready,
		"version": version, "updating": a.updating.Load(),
		"database_ok": databaseOK, "worker_running": workerRunning,
		"connected": connected, "logged_in": loggedIn, "status": status,
	})
}

func (a *app) enqueue(w http.ResponseWriter, r *http.Request) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err == nil && mediaType == "multipart/form-data" {
		a.enqueueMedia(w, r)
		return
	}
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
	a.queue(w, r, job{Number: request.Number, Message: request.Message, Type: "text"}, nil)
}

func (a *app) getJob(w http.ResponseWriter, r *http.Request) {
	j, err := readJob(a.db.QueryRowContext(r.Context(), jobSelect+` WHERE j.id=?`, r.PathValue("id")))
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
