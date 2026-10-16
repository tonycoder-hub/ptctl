package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
)

const maxPromptAnswerBytes = 4 << 10

// promptTerminal reads answers to prompts from an interactive terminal.
// ReadSecret turns echo off before it calls showPrompt, so nothing typed after
// the prompt appears is echoed.
type promptTerminal interface {
	ReadLine() (string, error)
	ReadSecret(showPrompt func()) (string, error)
}

// stdinTerminal returns stdin as a prompt terminal when it is one.
func stdinTerminal(reader io.Reader) (promptTerminal, bool) {
	file, ok := reader.(*os.File)
	if !ok {
		return nil, false
	}
	if _, err := getTerminalMode(file.Fd()); err != nil {
		return nil, false
	}
	return fileTerminal{file: file}, true
}

func (a *app) promptTerminal() (promptTerminal, bool) {
	if a.terminal != nil {
		return a.terminal(a.stdin)
	}
	return stdinTerminal(a.stdin)
}

type fileTerminal struct{ file *os.File }

// ReadLine reads one echoed line a byte at a time, so nothing typed after the
// newline is buffered away from a later ReadSecret.
func (t fileTerminal) ReadLine() (string, error) {
	var line []byte
	var b [1]byte
	for {
		n, err := t.file.Read(b[:])
		if n == 1 {
			if b[0] == '\n' {
				return strings.TrimSuffix(string(line), "\r"), nil
			}
			if len(line) == maxPromptAnswerBytes {
				return "", errors.New("prompt answer is too long")
			}
			line = append(line, b[0])
			continue
		}
		if err == io.EOF && len(line) > 0 {
			return strings.TrimSuffix(string(line), "\r"), nil
		}
		if err != nil {
			return "", err
		}
	}
}

// ReadSecret reads one line with echo off. Line editing and Ctrl+C keep
// working, and an interrupt restores echo before the process exits.
func (t fileTerminal) ReadSecret(showPrompt func()) (string, error) {
	mode, err := getTerminalMode(t.file.Fd())
	if err != nil {
		return "", err
	}
	interrupts := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(interrupts, os.Interrupt)
	go func() {
		select {
		case <-interrupts:
			_ = mode.restore()
			fmt.Fprintln(os.Stderr)
			os.Exit(130)
		case <-done:
		}
	}()
	defer func() {
		signal.Stop(interrupts)
		close(done)
	}()
	if err := mode.hideInput(); err != nil {
		return "", err
	}
	defer mode.restore()
	showPrompt()
	return t.ReadLine()
}
