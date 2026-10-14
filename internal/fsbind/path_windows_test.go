//go:build windows

package fsbind

import (
	"errors"
	"strings"
	"testing"
)

func TestWindowsComponentLimitCountsUTF16CodeUnits(t *testing.T) {
	tests := []struct {
		name      string
		component string
		valid     bool
	}{
		{name: "255 ascii", component: strings.Repeat("a", 255), valid: true},
		{name: "256 ascii", component: strings.Repeat("a", 256), valid: false},
		{name: "255 units with surrogate pairs", component: strings.Repeat("😀", 127) + "a", valid: true},
		{name: "256 units with surrogate pairs", component: strings.Repeat("😀", 128), valid: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := PathFromComponents([]string{test.component})
			if test.valid && err != nil {
				t.Fatalf("valid component rejected: %v", err)
			}
			if !test.valid && !errors.Is(err, ErrInvalidPath) {
				t.Fatalf("oversized component error = %v", err)
			}
		})
	}
}

func TestWindowsComponentRejectsEveryReservedDeviceSpelling(t *testing.T) {
	for _, component := range []string{"CONIN$", "conout$.txt", "COM0", "lpt0.bin", "COM¹.txt", "CON .txt", "nul.tar.gz", "Aux.log"} {
		if _, err := PathFromComponents([]string{component}); !errors.Is(err, ErrInvalidPath) {
			t.Fatalf("reserved device component %q error = %v", component, err)
		}
	}
	for _, component := range []string{"CONSOLE.txt", "COM10", "my.CON", "release"} {
		if _, err := PathFromComponents([]string{component}); err != nil {
			t.Fatalf("ordinary component %q rejected: %v", component, err)
		}
	}
}
