package cli

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/json"
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
	if code != 3 || !strings.Contains(out.String(), `"verified": false`) || !strings.Contains(errOut.String(), "failed exact torrent verification") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
}

func TestVerifyV2MismatchUsesIntegrityExitCode(t *testing.T) {
	root := t.TempDir()
	expected := sha256.Sum256([]byte("x"))
	metafile := testV2Metafile(expected)
	torrentPath := filepath.Join(root, "mismatch-v2.torrent")
	contentPath := filepath.Join(root, "content-v2.bin")
	if err := os.WriteFile(torrentPath, metafile, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(contentPath, []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	code := Run([]string{"torrent", "verify", "--content", contentPath, "--output", "json", torrentPath}, strings.NewReader(""), &out, &errOut)
	if code != 3 || !strings.Contains(out.String(), `"algorithm": "bt-v2"`) || !strings.Contains(out.String(), `"verified": false`) || !strings.Contains(errOut.String(), "failed exact torrent verification") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
}

func TestSeedPlanV2IntegrityExitAndMachineSafetyFields(t *testing.T) {
	root := t.TempDir()
	expected := sha256.Sum256([]byte("x"))
	torrentPath := filepath.Join(root, "v2.torrent")
	contentPath := filepath.Join(root, "content.bin")
	targetPath := t.TempDir()
	if err := os.WriteFile(torrentPath, testV2Metafile(expected), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(contentPath, []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	code := Run([]string{"seed", "plan", "--torrent", torrentPath, "--source", contentPath, "--target", targetPath, "--output", "json"}, strings.NewReader(""), &out, &errOut)
	if code != 3 || out.Len() != 0 || !strings.Contains(errOut.String(), "failed exact torrent verification") {
		t.Fatalf("mismatch code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}

	if err := os.WriteFile(contentPath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	code = Run([]string{"seed", "plan", "--torrent", torrentPath, "--source", contentPath, "--target", targetPath, "--output", "json"}, strings.NewReader(""), &out, &errOut)
	if code != 0 {
		t.Fatalf("valid code=%d stderr=%q", code, errOut.String())
	}
	var envelope struct {
		Data struct {
			Effect       string `json:"effect"`
			ReadyToApply bool   `json:"ready_to_apply"`
			Readiness    string `json:"readiness"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Data.Effect != "none" || envelope.Data.ReadyToApply || envelope.Data.Readiness != "layout_only" {
		t.Fatalf("unsafe or missing machine safety fields: %s", out.String())
	}

	out.Reset()
	errOut.Reset()
	code = Run([]string{"seed", "plan", "--torrent", torrentPath, "--source", contentPath, "--target", targetPath}, strings.NewReader(""), &out, &errOut)
	if code != 0 || strings.Index(out.String(), "BLOCKERS") < 0 || strings.Index(out.String(), "PLANNED ACTION") < strings.Index(out.String(), "BLOCKERS") {
		t.Fatalf("unclear plan output: code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
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

func testV2Metafile(root [32]byte) []byte {
	metafile := []byte("d4:infod9:file treed1:xd0:d6:lengthi1e11:pieces root32:")
	metafile = append(metafile, root[:]...)
	return append(metafile, []byte("eee12:meta versioni2e4:name1:x12:piece lengthi16384eee")...)
}
