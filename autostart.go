package main

import (
	"bytes"
	"encoding/xml"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func boolSetting(name string, fallback bool) (bool, error) {
	value, err := strconv.ParseBool(env(name, strconv.FormatBool(fallback)))
	if err != nil {
		return false, errors.New(name + " must be true or false")
	}
	return value, nil
}

func temporaryExecutable(executable string) bool {
	path := filepath.ToSlash(executable)
	return strings.Contains(path, "/go-build") && strings.Contains(path, "/exe/")
}

func writeStartupFile(path string, content []byte, enabled bool) error {
	if !enabled {
		err := os.Remove(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	// Do not rewrite unchanged registration files on every application launch.
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, content) {
		return nil
	}
	return os.WriteFile(path, content, 0600)
}

func desktopStartup(executable, configDir string) []byte {
	// Desktop Exec has its own escaping rules (it is not a shell command).
	quote := func(value string) string {
		value = strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "`", "\\`", "$", "\\$", "%", "%%").Replace(value)
		// Backslashes are escaped a second time at the desktop-entry string layer.
		return "\"" + strings.ReplaceAll(value, "\\", "\\\\") + "\""
	}
	path := strings.NewReplacer("\\", "\\\\", "\n", "\\n", "\r", "\\r").Replace(configDir)
	return []byte("[Desktop Entry]\nType=Application\nName=Smartfico WhatsApp\nExec=" + quote(executable) + " --startup\nPath=" + path + "\nTerminal=false\nX-GNOME-Autostart-enabled=true\n")
}

func launchAgentStartup(executable, configDir, stateDir string) []byte {
	escape := func(value string) string {
		var b bytes.Buffer
		_ = xml.EscapeText(&b, []byte(value))
		return b.String()
	}
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>app.smartfico.whatsapp</string>
<key>ProgramArguments</key><array><string>` + escape(executable) + `</string><string>--startup</string></array>
<key>WorkingDirectory</key><string>` + escape(configDir) + `</string>
<key>RunAtLoad</key><true/>
<key>StandardOutPath</key><string>` + escape(filepath.Join(stateDir, "service.log")) + `</string>
<key>StandardErrorPath</key><string>` + escape(filepath.Join(stateDir, "service.log")) + `</string>
</dict></plist>
`)
}
