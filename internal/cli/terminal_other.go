//go:build !(aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris || zos || windows)

package cli

import "errors"

var errNoHiddenInput = errors.New("hidden terminal input is not supported on this platform")

// terminalMode is unavailable here, so site login needs
// --credential-bundle-stdin.
type terminalMode struct{}

func getTerminalMode(uintptr) (*terminalMode, error) { return nil, errNoHiddenInput }

func (*terminalMode) hideInput() error { return errNoHiddenInput }

func (*terminalMode) restore() error { return nil }
