//go:build !unix

package typesafe

import "os"

// openKeyFile is unimplemented off Unix.
//
// The credential checks this package relies on (O_NOFOLLOW, a 0600 mode, and
// a uid comparison) do not have faithful equivalents elsewhere, and a partial
// version would claim protection it does not provide. The project targets
// macOS; this file exists so the package still builds and vets on other
// platforms rather than silently weakening the check there.
func openKeyFile(string) (*os.File, error) {
	return nil, badKeyFile("credential files are only supported on Unix systems")
}
