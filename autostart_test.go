package main

import (
	"bytes"
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestStartupFileInstallUpdateAndDisable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "autostart", "smartfico.desktop")
	if err := writeStartupFile(path, []byte("first"), true); err != nil {
		t.Fatal(err)
	}
	if err := writeStartupFile(path, []byte("second"), true); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "second" {
		t.Fatal(string(data), err)
	}
	if err := writeStartupFile(path, nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("startup file was not removed", err)
	}
	if err := writeStartupFile(path, nil, false); err != nil {
		t.Fatal("disable must be idempotent", err)
	}
}

func TestStartupFilesPreservePaths(t *testing.T) {
	text := string(desktopStartup(`/opt/Smart fico/$app%/smartfico-whatsapp`, `/opt/Smart fico`))
	if !strings.Contains(text, `Exec="/opt/Smart fico/\\$app%%/smartfico-whatsapp" --startup`) || !strings.Contains(text, "Terminal=false") {
		t.Fatal(text)
	}
	data := launchAgentStartup(`/Applications/Smart & Fico/whatsapp`, `/Applications/Smart & Fico`, `/Users/test/Library/Caches/test`)
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var elements []string
	for {
		token, err := decoder.Token()
		if err != nil {
			if err != io.EOF {
				t.Fatal(err)
			}
			break
		}
		if element, ok := token.(xml.StartElement); ok && element.Name.Local == "string" {
			var value string
			if err := decoder.DecodeElement(&value, &element); err != nil {
				t.Fatal(err)
			}
			elements = append(elements, value)
		}
	}
	if len(elements) < 3 || elements[1] != `/Applications/Smart & Fico/whatsapp` || elements[2] != "--startup" {
		t.Fatal(elements)
	}
}

func TestLinuxLoginRegistrationUsesUserConfigDirectory(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux desktop autostart")
	}
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := syncAutostart("/opt/smartfico/whatsapp", "/opt/smartfico", t.TempDir(), true); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "autostart", "smartfico-whatsapp.desktop")
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if err := syncAutostart("/opt/smartfico/whatsapp", "/opt/smartfico", t.TempDir(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestTemporaryGoRunDoesNotRegister(t *testing.T) {
	if !temporaryExecutable("/tmp/go-build1234/b001/exe/smartfico") || temporaryExecutable("/opt/smartfico/smartfico-whatsapp") {
		t.Fatal("temporary executable detection failed")
	}
}
