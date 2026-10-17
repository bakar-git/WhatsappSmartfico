//go:build unix

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

func syncAutostart(executable, configDir, stateDir string, enabled bool) error {
	switch runtime.GOOS {
	case "linux":
		config, err := os.UserConfigDir()
		if err != nil {
			return err
		}
		return writeStartupFile(filepath.Join(config, "autostart", "smartfico-whatsapp.desktop"), desktopStartup(executable, configDir), enabled)
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		return writeStartupFile(filepath.Join(home, "Library", "LaunchAgents", "app.smartfico.whatsapp.plist"), launchAgentStartup(executable, configDir, stateDir), enabled)
	default:
		return fmt.Errorf("login startup is not supported on %s", runtime.GOOS)
	}
}

func hideStartupConsole() {}
