package typesafe

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// writeKeyFile creates a credential file with the given contents and mode.
// The contents are always the fake sentinel or a deliberately malformed value;
// no real credential appears anywhere in this package's tests.
func writeKeyFile(t *testing.T, contents string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jev-pi.key")
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatalf("write key fixture: %v", err)
	}
	// WriteFile applies the umask, so set the mode explicitly.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod key fixture: %v", err)
	}
	return path
}

func TestReadKeyAcceptsAWellFormedFile(t *testing.T) {
	for name, contents := range map[string]string{
		"bare token":       fakeKey,
		"trailing newline": fakeKey + "\n",
		"trailing CRLF":    fakeKey + "\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := ReadKey(writeKeyFile(t, contents, 0o600))
			if err != nil {
				t.Fatalf("ReadKey: %v", err)
			}
			if got != fakeKey {
				t.Fatalf("got %q, want the sentinel", got)
			}
		})
	}
}

func TestReadKeyRejectsBadModes(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o400, 0o660, 0o700} {
		path := writeKeyFile(t, fakeKey, mode)
		if _, err := ReadKey(path); !errors.Is(err, ErrKeyFile) {
			t.Errorf("mode %o should be rejected, got %v", mode, err)
		}
	}
}

func TestReadKeyRejectsBadPaths(t *testing.T) {
	dir := t.TempDir()
	good := writeKeyFile(t, fakeKey, 0o600)

	cases := map[string]string{
		"empty":          "",
		"relative":       "jev-pi.key",
		"dot relative":   "./jev-pi.key",
		"parent escape":  filepath.Dir(good) + "/../" + filepath.Base(good),
		"doubled slash":  filepath.Dir(good) + "//" + filepath.Base(good),
		"trailing slash": good + "/",
		"missing":        filepath.Join(dir, "absent.key"),
		"directory":      dir,
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadKey(path); !errors.Is(err, ErrKeyFile) {
				t.Fatalf("want ErrKeyFile, got %v", err)
			}
		})
	}
}

// A symlink planted where the key file belongs must fail rather than silently
// redirect the read. O_NOFOLLOW is what enforces this.
func TestReadKeyRejectsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	target := writeKeyFile(t, fakeKey, 0o600)
	link := filepath.Join(t.TempDir(), "link.key")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if _, err := ReadKey(link); !errors.Is(err, ErrKeyFile) {
		t.Fatalf("want ErrKeyFile, got %v", err)
	}
}

func TestReadKeyRejectsBadContents(t *testing.T) {
	cases := map[string]string{
		"empty":            "",
		"newline only":     "\n",
		"too short":        "abc",
		"internal space":   "abc def ghijklmno",
		"leading space":    " " + fakeKey,
		"two lines":        fakeKey + "\nsecond-line-here",
		"internal tab":     "abc\tdefghijklmno",
		"non-ASCII":        "naïve-token-with-an-accent",
		"control byte":     "abcdefgh\x01ijkl",
		"over the cap":     repeat(maxKeyBytes + 1),
		"exactly over cap": repeat(maxKeyBytes+1) + "\n",
	}
	for name, contents := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadKey(writeKeyFile(t, contents, 0o600)); !errors.Is(err, ErrKeyFile) {
				t.Fatalf("want ErrKeyFile, got %v", err)
			}
		})
	}
}

func TestReadKeyAcceptsExactlyTheCap(t *testing.T) {
	token := repeat(maxKeyBytes)
	got, err := ReadKey(writeKeyFile(t, token, 0o600))
	if err != nil {
		t.Fatalf("a %d byte token should be accepted, got %v", maxKeyBytes, err)
	}
	if got != token {
		t.Fatal("token was altered")
	}
}

// The credential file's location and contents are exactly what must not end up
// in an MCP tool result or a session transcript.
func TestReadKeyErrorsRevealNothing(t *testing.T) {
	path := writeKeyFile(t, fakeKey+" trailing garbage", 0o644)
	_, err := ReadKey(path)
	assertNoLeak(t, err, path, filepath.Dir(path), fakeKey, "trailing garbage")
}

func TestParseKeyRejectsMultipleTrailingNewlines(t *testing.T) {
	if _, err := parseKey([]byte(fakeKey + "\n\n")); !errors.Is(err, ErrKeyFile) {
		t.Fatalf("want ErrKeyFile, got %v", err)
	}
}
