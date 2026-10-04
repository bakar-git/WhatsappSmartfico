package main

import (
	"net/url"
	"path/filepath"
	"strings"
)

func sqliteDSN(path string) string {
	uriPath := filepath.ToSlash(path)
	// SQLite expects a Windows drive path as /D:/folder/file.db. Without
	// the leading slash, net/url can put D: and the path in the authority.
	if len(path) >= 3 && ((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')) &&
		path[1] == ':' && (path[2] == '\\' || path[2] == '/') {
		uriPath = "/" + strings.ReplaceAll(path, "\\", "/")
	}
	return (&url.URL{
		Scheme: "file", Path: uriPath,
		RawQuery: "_foreign_keys=on&_busy_timeout=5000&_journal_mode=WAL",
	}).String()
}
