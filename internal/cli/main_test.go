package cli

import (
	"fmt"
	"os"
	"testing"
)

// TestMain points the user config directory at an empty temporary one, so no
// test ever reads or sends a credential 'site login' saved for the real user.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "ptctl-cli-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "create test home:", err)
		os.Exit(1)
	}
	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "AppData"} {
		if err := os.Setenv(name, home); err != nil {
			fmt.Fprintln(os.Stderr, "isolate test home:", err)
			os.Exit(1)
		}
	}
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}
