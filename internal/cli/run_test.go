package cli

import (
	"bytes"
	"crypto/sha1"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type trackingReader struct{ read bool }

func (r *trackingReader) Read([]byte) (int, error) {
	r.read = true
	return 0, fmt.Errorf("unexpected secret read")
}

func TestHelpAndSiteList(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Run([]string{"site", "list"}, strings.NewReader(""), &out, &errOut); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "tjupt") || errOut.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q", out.String(), errOut.String())
	}
}

func TestCredentialMustNotBeAcceptedAsArgument(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Run([]string{"site", "status", "tjupt"}, strings.NewReader(""), &out, &errOut)
	if code != 2 || !strings.Contains(errOut.String(), "--cookie-stdin") {
		t.Fatalf("code=%d stderr=%q", code, errOut.String())
	}
}

func TestInvalidOutputAndMissingCapabilityDoNotReadSecrets(t *testing.T) {
	for _, args := range [][]string{
		{"site", "status", "--cookie-stdin", "--output", "yaml", "tjupt"},
		{"site", "account", "--cookie-stdin", "tjupt"},
		{"client", "status", "--url", "https://seedbox.invalid", "--password-stdin", "--output", "yaml"},
	} {
		reader := &trackingReader{}
		var out, errOut bytes.Buffer
		if code := Run(args, reader, &out, &errOut); code == 0 {
			t.Fatalf("expected failure for %v", args)
		}
		if reader.read {
			t.Fatalf("secret input was read for rejected command %v", args)
		}
	}
}

func TestVerifyMismatchUsesIntegrityExitCode(t *testing.T) {
	root := t.TempDir()
	expected := sha1.Sum([]byte("x"))
	metafile := append([]byte("d4:infod6:lengthi1e4:name1:x12:piece lengthi1e6:pieces20:"), expected[:]...)
	metafile = append(metafile, 'e', 'e')
	torrentPath := filepath.Join(root, "mismatch.torrent")
	contentPath := filepath.Join(root, "content.bin")
	if err := os.WriteFile(torrentPath, metafile, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(contentPath, []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	code := Run([]string{"torrent", "verify", "--content", contentPath, "--output", "json", torrentPath}, strings.NewReader(""), &out, &errOut)
	if code != 3 || !strings.Contains(out.String(), `"verified": false`) || !strings.Contains(errOut.String(), "failed exact piece verification") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
}

func TestInspectEscapesTerminalControlSequences(t *testing.T) {
	root := t.TempDir()
	name := "evil\x1b]52;c;Y2FuYXJ5\a\nname"
	doc := []byte("d4:infod6:lengthi0e4:name" + strconv.Itoa(len(name)) + ":" + name + "12:piece lengthi1e6:pieces0:ee")
	path := filepath.Join(root, "unsafe.torrent")
	if err := os.WriteFile(path, doc, 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"torrent", "inspect", path}, strings.NewReader(""), &out, &errOut); code != 1 {
		t.Fatalf("code=%d stderr=%q", code, errOut.String())
	}
	if strings.ContainsAny(out.String()+errOut.String(), "\x1b\a") {
		t.Fatalf("unsafe parser diagnostic: stdout=%q stderr=%q", out.String(), errOut.String())
	}
	escaped := terminalSafe(name)
	if strings.ContainsAny(escaped, "\x1b\a\n") || !strings.Contains(escaped, `\x1b`) || !strings.Contains(escaped, `\n`) {
		t.Fatalf("unsafe or missing terminal escaping: %q", escaped)
	}
}
