package typesafe

import (
	"io"
	"path/filepath"
	"strings"
)

// ReadKey loads the API credential from a dedicated key file.
//
// The credential is read from a file the owner provisioned, never from the
// environment: an inherited TYPESAFE_API_KEY would be visible to every child
// process in the tree and would make which key is in use depend on who started
// the server.
//
// The file must be an absolute, cleaned path naming a regular file (not a
// symlink, not a FIFO, not a device), owned by the current user, with mode
// exactly 0600, holding a single printable ASCII token of at most 256 bytes
// with an optional trailing newline.
//
// Two of those checks deserve a note. The type and permission checks run
// against the open file descriptor, not the path, so the file that was
// inspected is the file that was read; a swap between the two cannot change
// what is accepted. And the file is opened non-blocking, so naming a FIFO
// fails immediately instead of parking the server on an open that never
// returns.
//
// Mode 0600 is worth being honest about: it keeps the key from other users on
// the machine. It does nothing against another process running as this same
// user. It is a floor, not isolation.
//
// On failure the error wraps [ErrKeyFile] and never names the path, the
// credential, or any part of the file's contents.
func ReadKey(path string) (string, error) {
	if path == "" {
		return "", badKeyFile("no path was given")
	}
	if !filepath.IsAbs(path) {
		return "", badKeyFile("path must be absolute")
	}
	// A path that is not already clean contains "..", a doubled separator, or
	// a trailing slash. Refuse it rather than silently reading whatever it
	// resolves to.
	if filepath.Clean(path) != path {
		return "", badKeyFile("path must be in canonical form")
	}

	f, err := openKeyFile(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	// Read one byte past the limit so an oversized file is detected rather
	// than silently truncated into a key prefix.
	raw, err := io.ReadAll(io.LimitReader(f, maxKeyBytes+1))
	if err != nil {
		return "", badKeyFile("file could not be read")
	}
	return parseKey(raw)
}

// parseKey turns the file's bytes into a credential, or explains why it cannot.
func parseKey(raw []byte) (string, error) {
	if len(raw) > maxKeyBytes {
		return "", badKeyFile("file is larger than a credential should be")
	}
	s := string(raw)

	// Exactly one optional trailing newline: a text editor adds one, and
	// refusing it would be pedantic. Anything beyond that is not a single
	// token on a single line.
	s = strings.TrimSuffix(s, "\n")
	s = strings.TrimSuffix(s, "\r")

	if s == "" {
		return "", badKeyFile("file is empty")
	}
	if len(s) < minKeyBytes {
		return "", badKeyFile("file does not contain a credential")
	}
	if !safeToken(s) {
		// Covers embedded whitespace, extra lines, and any non-printable or
		// non-ASCII byte. Deliberately vague about which: the reason would
		// describe the credential.
		return "", badKeyFile("file must contain a single printable token on one line")
	}
	return s, nil
}
