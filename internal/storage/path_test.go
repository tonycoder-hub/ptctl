package storage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRejectDangerousWindowsPaths(t *testing.T) {
	semantics := PathSemantics{Windows: true, CaseSensitive: false, UnicodeNormalization: true}
	bad := [][][]byte{
		{[]byte("..")},
		{[]byte("CON")},
		{[]byte("movie:stream")},
		{[]byte("trailing.")},
		{[]byte("a/b")},
		{[]byte("e\u0301")},
		{[]byte("bad?.mkv")},
		{[]byte("bad\x1b.mkv")},
		{[]byte("COM¹.txt")},
	}
	for _, path := range bad {
		if err := ValidateComponents(path, semantics); err == nil {
			t.Fatalf("expected rejection for %q", path)
		}
	}
}

func TestWindowsReservedDeviceNames(t *testing.T) {
	semantics := PathSemantics{Windows: true, CaseSensitive: false}
	for _, name := range []string{
		"CON", "con.txt", "CON .txt", "Con  .tar.gz", "nul.", "AUX", "PRN.log",
		"CONIN$", "conin$.txt", "CONOUT$", "conout$ .log", "CLOCK$",
		"COM1", "com9.txt", "COM0", "LPT0.log", "COM¹", "com².txt", "LPT³", "lpt¹ .txt",
	} {
		if !windowsReserved(name) {
			t.Fatalf("%q was not treated as a reserved device name", name)
		}
		if err := ValidateComponents([][]byte{[]byte(name)}, semantics); err == nil {
			t.Fatalf("%q was accepted as a Windows path component", name)
		}
	}
	for _, name := range []string{
		"CONSOLE", "CON_", " CON", "CONIN", "CONOUT", "COM10", "LPT", "COM", "COM⁴", "COM1x.txt", "x.CON", "NULL.txt",
	} {
		if windowsReserved(name) {
			t.Fatalf("%q was treated as a reserved device name", name)
		}
		if err := ValidateComponents([][]byte{[]byte(name)}, semantics); err != nil {
			t.Fatalf("%q was rejected: %v", name, err)
		}
	}
}

func TestSecureJoinExistingRejectsLinkTraversal(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "real")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SecureJoinExisting(root, [][]byte{[]byte("real"), []byte("file")}, CurrentSemantics()); err != nil {
		t.Fatalf("normal path rejected: %v", err)
	}
	linkedDirectory := filepath.Join(root, "linked")
	if err := os.Symlink(directory, linkedDirectory); err == nil {
		if _, err := SecureJoinExisting(root, [][]byte{[]byte("linked"), []byte("file")}, CurrentSemantics()); err == nil {
			t.Fatal("internal symbolic-link traversal was accepted")
		}
	}
	linkedRoot := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(root, linkedRoot); err == nil {
		if _, err := SecureJoinExisting(linkedRoot, [][]byte{[]byte("real"), []byte("file")}, CurrentSemantics()); err == nil {
			t.Fatal("symbolic-link storage root was accepted")
		}
	}
}

func TestRejectFileDirectoryPrefixCollision(t *testing.T) {
	paths := [][][]byte{{[]byte("a")}, {[]byte("a"), []byte("b.mkv")}}
	if err := ValidateManifestPaths(paths, PathSemantics{CaseSensitive: true}); err == nil {
		t.Fatal("expected file/directory prefix collision")
	}
}

func TestManifestPathsAllowOnlyPaddingToRepeat(t *testing.T) {
	pad := [][]byte{[]byte(".pad"), []byte("16383")}
	media := [][]byte{[]byte("a.mkv")}
	semantics := PathSemantics{CaseSensitive: true}
	if err := ValidateManifestPathsWithPadding([][][]byte{media, pad, {[]byte("b.mkv")}, pad}, []bool{false, true, false, true}, semantics); err != nil {
		t.Fatalf("repeated BEP 47 padding paths rejected: %v", err)
	}
	if err := ValidateManifestPaths([][][]byte{pad, pad}, semantics); err == nil {
		t.Fatal("ValidateManifestPaths accepted a repeated path")
	}
	folded := [][][]byte{pad, {[]byte(".PAD"), []byte("16383")}}
	if err := ValidateManifestPathsWithPadding(folded, []bool{true, true}, PathSemantics{Windows: true}); err != nil {
		t.Fatalf("case-folded padding paths rejected: %v", err)
	}
	for name, test := range map[string]struct {
		paths   [][][]byte
		padding []bool
	}{
		"padding then real file": {[][][]byte{pad, pad}, []bool{true, false}},
		"real file then padding": {[][][]byte{media, media}, []bool{false, true}},
		"padding prefix":         {[][][]byte{{[]byte(".pad")}, pad}, []bool{true, true}},
		"unsafe padding":         {[][][]byte{{[]byte(".."), []byte("1")}}, []bool{true}},
		"flag count mismatch":    {[][][]byte{pad, pad}, []bool{true}},
	} {
		if err := ValidateManifestPathsWithPadding(test.paths, test.padding, semantics); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

func TestDarwinLetterlikeSingletonsCollideThroughCaseFolding(t *testing.T) {
	// Without NFD tables, the Kelvin, Ohm, and Angstrom signs are still caught
	// because their lowercase mappings equal the decomposed letters' mappings.
	darwin := PathSemantics{CaseSensitive: false, UnicodeNormalization: true}
	for _, pair := range [][2]string{{"\u212bngstrom", "\u00c5ngstrom"}, {"\u2126", "\u03a9"}, {"\u212a.bin", "K.bin"}} {
		if err := ValidateManifestPaths([][][]byte{{[]byte(pair[0])}, {[]byte(pair[1])}}, darwin); err == nil {
			t.Fatalf("%q and %q did not collide", pair[0], pair[1])
		}
	}
}

func TestDetectCaseCollision(t *testing.T) {
	paths := [][][]byte{{[]byte("A.mkv")}, {[]byte("a.mkv")}}
	if err := ValidateManifestPaths(paths, PathSemantics{Windows: true, CaseSensitive: false}); err == nil {
		t.Fatal("expected collision")
	}
}

func TestDetectWindowsUnicodeSimpleFoldCollision(t *testing.T) {
	paths := [][][]byte{{[]byte("σ.mkv")}, {[]byte("ς.mkv")}}
	if err := ValidateManifestPaths(paths, PathSemantics{Windows: true, CaseSensitive: false}); err == nil {
		t.Fatal("expected sigma/final-sigma collision")
	}
	if err := ValidateManifestPaths(paths, PathSemantics{Windows: true, CaseSensitive: true}); err != nil {
		t.Fatalf("case-sensitive paths unexpectedly collided: %v", err)
	}
}

func TestDetectWindowsSimpleFoldPrefixCollisionWithInterposition(t *testing.T) {
	paths := [][][]byte{
		{[]byte("Σ")},
		{[]byte("Σ-branch")},
		{[]byte("ς"), []byte("child.mkv")},
	}
	if err := ValidateManifestPaths(paths, PathSemantics{Windows: true, CaseSensitive: false}); err == nil {
		t.Fatal("lexical interposition hid a simple-fold file/directory prefix collision")
	}
}

func TestMapHostToClient(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "movies", "a.mkv")
	got, err := MapHostToClient(root, path, "/downloads", false)
	if err != nil {
		t.Fatal(err)
	}
	if got.ClientPath != "/downloads/movies/a.mkv" {
		t.Fatalf("client path = %q", got.ClientPath)
	}
}

func TestMapHostToClientRejectsAmbiguousClientRoots(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.mkv")
	for _, test := range []struct {
		root    string
		windows bool
	}{
		{root: `\\downloads`, windows: false},
		{root: `\\server\`, windows: true},
		{root: `downloads`, windows: false},
		{root: `C:downloads`, windows: true},
	} {
		if _, err := MapHostToClient(root, path, test.root, test.windows); err == nil {
			t.Fatalf("expected client root %q to be rejected", test.root)
		}
	}
}

func TestMapHostToClientPreservesFilesystemRoots(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "a.mkv")
	posix, err := MapHostToClient(root, path, "/", false)
	if err != nil {
		t.Fatal(err)
	}
	if posix.ClientPath != "/a.mkv" {
		t.Fatalf("POSIX root mapping = %q", posix.ClientPath)
	}
	windows, err := MapHostToClient(root, path, `C:\`, true)
	if err != nil {
		t.Fatal(err)
	}
	if windows.ClientPath != `C:\a.mkv` {
		t.Fatalf("Windows root mapping = %q", windows.ClientPath)
	}
}

func TestMapHostToClientCanonicalizesParentAlias(t *testing.T) {
	container := t.TempDir()
	actualParent := filepath.Join(container, "actual")
	root := filepath.Join(actualParent, "root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	aliasParent := filepath.Join(container, "alias")
	if err := os.Symlink(actualParent, aliasParent); err != nil {
		t.Skipf("cannot create directory symlink: %v", err)
	}
	inputRoot := filepath.Join(aliasParent, "root")
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(root, "existing.bin")
	if err := os.WriteFile(existing, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	mapped, err := MapHostToClient(inputRoot, existing, "/downloads", false)
	if err != nil {
		t.Fatal(err)
	}
	if mapped.ClientPath != "/downloads/existing.bin" || mapped.HostRoot != canonicalRoot {
		t.Fatalf("existing canonical path mapping = %#v", mapped)
	}

	planned, err := MapHostToClient(inputRoot, filepath.Join(inputRoot, "planned.bin"), "/downloads", false)
	if err != nil {
		t.Fatal(err)
	}
	if planned.ClientPath != "/downloads/planned.bin" || planned.HostPath != filepath.Join(canonicalRoot, "planned.bin") {
		t.Fatalf("planned alias path mapping = %#v", planned)
	}
}

func TestMapHostToClientRejectsPlannedPathThroughLink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	linkedDirectory := filepath.Join(root, "linked")
	if err := os.Symlink(outside, linkedDirectory); err != nil {
		t.Skipf("cannot create directory symlink: %v", err)
	}
	planned := filepath.Join(linkedDirectory, "planned.bin")
	if _, err := MapHostToClient(root, planned, "/downloads", false); err == nil {
		t.Fatal("planned path through a symbolic link was accepted")
	}
}
