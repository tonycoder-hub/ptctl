//go:build linux

package fsbind

import (
	"errors"
	"strings"
	"testing"
)

func TestLinuxComponentLimitCountsBytes(t *testing.T) {
	tests := []struct {
		name      string
		component string
		valid     bool
	}{
		{name: "255 ascii bytes", component: strings.Repeat("a", 255), valid: true},
		{name: "256 ascii bytes", component: strings.Repeat("a", 256), valid: false},
		{name: "255 multibyte bytes", component: strings.Repeat("é", 127) + "a", valid: true},
		{name: "256 multibyte bytes", component: strings.Repeat("é", 128), valid: false},
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
