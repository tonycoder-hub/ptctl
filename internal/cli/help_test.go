package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestReadOnlyHelpDiscovery(t *testing.T) {
	cases := []struct{ topic, want string }{
		{"torrent", "pt torrent inspect"}, {"client", "pt client status"},
		{"reconcile", "local-only report is partial"},
		{"config", "no automatic discovery"}, {"version", "-output"},
		{"torrent inspect", "-metafile-variant"}, {"torrent verify", "-content"},
		{"client list", "-password-stdin"}, {"client status", "-driver"},
		{"reconcile report", "require-reconciled"},
	}
	for _, tc := range cases {
		t.Run(tc.topic, func(t *testing.T) {
			reader := &trackingReader{}
			var out, errOut bytes.Buffer
			args := append([]string{"help"}, strings.Fields(tc.topic)...)
			if code := Run(args, reader, &out, &errOut); code != 0 || reader.read || errOut.Len() != 0 || !strings.Contains(out.String(), tc.want) {
				t.Fatalf("code=%d read=%t stdout=%q stderr=%q", code, reader.read, out.String(), errOut.String())
			}
		})
	}
	// A quoted multi-word topic must not index absent argv elements or panic.
	reader := &trackingReader{}
	var out, errOut bytes.Buffer
	if code := Run([]string{"help", "torrent inspect"}, reader, &out, &errOut); code != 0 || reader.read || errOut.Len() != 0 || !strings.Contains(out.String(), "-metafile-variant") {
		t.Fatalf("quoted topic code=%d read=%t stdout=%q stderr=%q", code, reader.read, out.String(), errOut.String())
	}
	for _, group := range []string{"torrent", "client", "reconcile"} {
		for _, flag := range []string{"-h", "--help", "help"} {
			reader := &trackingReader{}
			var out, errOut bytes.Buffer
			if code := Run([]string{group, flag}, reader, &out, &errOut); code != 0 || reader.read || errOut.Len() != 0 || !strings.HasPrefix(out.String(), "Usage:") {
				t.Fatalf("%s %s: code=%d read=%t stdout=%q stderr=%q", group, flag, code, reader.read, out.String(), errOut.String())
			}
		}
	}
}

func TestHelpNeverDispatchesArbitraryCommands(t *testing.T) {
	for _, args := range [][]string{
		{"help", "client", "remove", "run", "--password-stdin"},
		{"help", "torrent", "inspect", "missing.torrent"},
		{"help", "config", "unexpected"},
	} {
		reader := &trackingReader{}
		var out, errOut bytes.Buffer
		if code := Run(args, reader, &out, &errOut); code != 2 || reader.read || out.Len() != 0 || !strings.Contains(errOut.String(), "unknown help topic") {
			t.Fatalf("args=%v code=%d read=%t stdout=%q stderr=%q", args, code, reader.read, out.String(), errOut.String())
		}
	}
}

func TestMetafileFlagOrderHasActionableError(t *testing.T) {
	for _, args := range [][]string{
		{"torrent", "inspect", "missing.torrent", "--output", "json"},
		{"torrent", "verify", "--content", "missing.bin", "missing.torrent", "--output", "json"},
	} {
		reader := &trackingReader{}
		var out, errOut bytes.Buffer
		if code := Run(args, reader, &out, &errOut); code != 2 || reader.read || out.Len() != 0 || !strings.Contains(errOut.String(), "put options before FILE.torrent") {
			t.Fatalf("args=%v code=%d read=%t stdout=%q stderr=%q", args, code, reader.read, out.String(), errOut.String())
		}
	}
	// Missing arguments remain a usage error; a single leading-dash filename
	// already separated by -- is still a literal positional path.
	if _, err := positionalMetafileInput("torrent inspect", nil, "", "", false, false); err == nil {
		t.Fatal("missing metafile must fail")
	}
	input, err := positionalMetafileInput("torrent inspect", []string{"-literal.torrent"}, "", "", false, false)
	if err != nil || input.path != "-literal.torrent" {
		t.Fatalf("literal leading-dash path: input=%+v err=%v", input, err)
	}
}
