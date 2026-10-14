package fsbind

import "testing"

func TestWindowsReservedDeviceNameUsesFirstDotTrimmedCaseInsensitiveBase(t *testing.T) {
	for _, component := range []string{
		"CON", "con", "Prn", "aux.txt", "NUL.tar.gz", "clock$", "CONIN$", "conout$.log",
		"COM0", "com9", "LPT0", "lpt5.bin", "COM¹", "lpt²", "LPT³.txt",
		"CON .txt", "nul  .tar.gz", "Com1 .x.y", "con.", "AUX.",
	} {
		if !windowsReservedDeviceName(component) {
			t.Errorf("reserved device name %q was accepted", component)
		}
	}
	for _, component := range []string{
		"CONSOLE", "CON1", "NULL.txt", "COM", "LPT", "COM10", "LPT¹0", "COM⁴", "xCON", " CON",
		"conin", "my.CON", "data.nul", "COM1x.txt", "release", "aux_files", "",
	} {
		if windowsReservedDeviceName(component) {
			t.Errorf("ordinary name %q was rejected", component)
		}
	}
}
