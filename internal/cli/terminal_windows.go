//go:build windows

package cli

import "golang.org/x/sys/windows"

// terminalMode is the console input mode a hidden read restores.
type terminalMode struct {
	handle windows.Handle
	mode   uint32
}

func getTerminalMode(fd uintptr) (*terminalMode, error) {
	var mode uint32
	if err := windows.GetConsoleMode(windows.Handle(fd), &mode); err != nil {
		return nil, err
	}
	return &terminalMode{handle: windows.Handle(fd), mode: mode}, nil
}

// hideInput turns echo off and keeps console line editing and Ctrl+C.
func (m *terminalMode) hideInput() error {
	hidden := m.mode &^ windows.ENABLE_ECHO_INPUT
	hidden |= windows.ENABLE_LINE_INPUT | windows.ENABLE_PROCESSED_INPUT
	return windows.SetConsoleMode(m.handle, hidden)
}

func (m *terminalMode) restore() error {
	return windows.SetConsoleMode(m.handle, m.mode)
}
