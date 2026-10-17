package main

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

func syncAutostart(executable, configDir, stateDir string, enabled bool) error {
	key, _, err := registry.CreateKey(registry.CURRENT_USER, `Software\Microsoft\Windows\CurrentVersion\Run`, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	if !enabled {
		err := key.DeleteValue("SmartficoWhatsApp")
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return err
	}
	return key.SetStringValue("SmartficoWhatsApp", syscall.EscapeArg(executable)+" --startup")
}

func hideStartupConsole() {
	kernel := windows.NewLazySystemDLL("kernel32.dll")
	window, _, _ := kernel.NewProc("GetConsoleWindow").Call()
	if window != 0 {
		_, _, _ = windows.NewLazySystemDLL("user32.dll").NewProc("ShowWindow").Call(window, 0)
	}
}
