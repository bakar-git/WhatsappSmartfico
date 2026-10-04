package main

import (
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSQLiteWindowsDriveURI(t *testing.T) {
	for _, path := range []string{`D:\whatsapp\data\whatsapp.db`, "D:/whatsapp/data/whatsapp.db", `C:\Smartfico Receipts #1\data\whatsapp.db`} {
		u, err := url.Parse(sqliteDSN(path))
		if err != nil {
			t.Fatal(err)
		}
		if u.Scheme != "file" || u.Host != "" || u.Opaque != "" || u.Path != "/"+strings.ReplaceAll(path, "\\", "/") {
			t.Fatalf("Windows path became an invalid file URI: %s", sqliteDSN(path))
		}
		if u.Fragment != "" || u.Query().Get("_foreign_keys") != "on" {
			t.Fatalf("filename changed URI parameters: %s", u)
		}
	}
}

func TestSQLiteOpensExactPathWithRequiredSettings(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Receipts #1 % special")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "whatsapp.db")
	db, err := sql.Open("sqlite3", sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database created at the wrong path: %v", err)
	}
	var foreignKeys, timeout int
	var journal string
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`PRAGMA busy_timeout`).Scan(&timeout); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 || timeout != 5000 || journal != "wal" {
		t.Fatalf("SQLite settings lost: foreign_keys=%d timeout=%d journal=%s", foreignKeys, timeout, journal)
	}
}
