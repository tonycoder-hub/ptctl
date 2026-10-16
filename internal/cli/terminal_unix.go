//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris || zos

package cli

import "golang.org/x/sys/unix"

// terminalMode is the termios state a hidden read restores.
type terminalMode struct {
	fd      int
	termios unix.Termios
}

func getTerminalMode(fd uintptr) (*terminalMode, error) {
	termios, err := unix.IoctlGetTermios(int(fd), ioctlReadTermios)
	if err != nil {
		return nil, err
	}
	return &terminalMode{fd: int(fd), termios: *termios}, nil
}

// hideInput turns echo off and keeps canonical line editing and signals. It
// does not flush input that is already queued.
func (m *terminalMode) hideInput() error {
	hidden := m.termios
	hidden.Lflag &^= unix.ECHO
	hidden.Lflag |= unix.ICANON | unix.ISIG
	hidden.Iflag |= unix.ICRNL
	return unix.IoctlSetTermios(m.fd, ioctlWriteTermios, &hidden)
}

func (m *terminalMode) restore() error {
	return unix.IoctlSetTermios(m.fd, ioctlWriteTermios, &m.termios)
}
