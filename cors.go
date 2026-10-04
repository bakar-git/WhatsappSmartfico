package main

import (
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

var smartficoHost = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+smartfico\.(?:test|app)$`)

func allowedOrigin(origin string) bool {
	u, err := url.Parse(origin)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") &&
		u.User == nil && u.Path == "" && u.RawQuery == "" && !u.ForceQuery &&
		u.Fragment == "" && smartficoHost.MatchString(strings.ToLower(u.Hostname()))
}

// Reject untrusted browser origins before authentication or side effects.
// Preflights do not contain bearer tokens and must run before authentication.
func cors(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Add("Vary", "Origin")
	origin := r.Header.Get("Origin")
	if origin != "" {
		if !allowedOrigin(origin) {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "Origin is not allowed"})
			return false
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Expose-Headers", "Location")
	}
	if r.Method != http.MethodOptions {
		return true
	}
	w.Header().Add("Vary", "Access-Control-Request-Method")
	w.Header().Add("Vary", "Access-Control-Request-Headers")
	w.Header().Add("Vary", "Access-Control-Request-Private-Network")
	method := r.Header.Get("Access-Control-Request-Method")
	if origin == "" || (method != http.MethodGet && method != http.MethodPost) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "CORS method or origin is not allowed"})
		return false
	}
	for _, header := range strings.Split(r.Header.Get("Access-Control-Request-Headers"), ",") {
		switch strings.ToLower(strings.TrimSpace(header)) {
		case "", "authorization", "content-type":
		default:
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "CORS request header is not allowed"})
			return false
		}
	}
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	w.Header().Set("Access-Control-Max-Age", "600")
	if r.Header.Get("Access-Control-Request-Private-Network") == "true" {
		w.Header().Set("Access-Control-Allow-Private-Network", "true")
	}
	w.WriteHeader(http.StatusNoContent)
	return false
}
