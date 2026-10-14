package fsbind

import "strings"

// windowsReservedDeviceName reports whether a component would name a Win32
// device instead of a directory entry. Windows ignores everything from the
// first dot and any spaces before it, so the base is the part before the first
// dot with trailing spaces trimmed, compared case-insensitively. Win32 also
// treats COM and LPT with a superscript 1, 2, or 3 as devices. The rule is
// platform-neutral so every host can test what platform_windows.go enforces.
func windowsReservedDeviceName(component string) bool {
	base := component
	if dot := strings.IndexByte(base, '.'); dot >= 0 {
		base = base[:dot]
	}
	base = strings.ToUpper(strings.TrimRight(base, " "))
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CLOCK$", "CONIN$", "CONOUT$":
		return true
	}
	if strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT") {
		switch base[3:] {
		case "0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³":
			return true
		}
	}
	return false
}
