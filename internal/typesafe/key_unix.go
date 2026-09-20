//go:build unix

package typesafe

import (
	"os"
	"syscall"
)

// openKeyFile opens the credential file and verifies it through the resulting
// descriptor.
//
// O_NOFOLLOW refuses the open outright if the final path component is a
// symlink, so a link planted where the key file should be is an error rather
// than a redirect. O_NONBLOCK makes opening a FIFO return immediately instead
// of blocking until a writer appears; the fstat below then rejects it for not
// being a regular file. On a regular file O_NONBLOCK has no effect on reads.
//
// Every check runs on the open descriptor via Stat, not on the path via Lstat.
// A file swapped in after the open still has the inode that was checked, which
// is as much as a single-user process can do about the race without holding a
// lock the rest of the system does not take. Intermediate directories are not
// re-verified: a user who can rewrite their own ~/.config already controls what
// the server reads.
func openKeyFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		// os.OpenFile's error embeds the path, so it is dropped here.
		return nil, badKeyFile("file is missing, unreadable, or is a symbolic link")
	}

	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, badKeyFile("file could not be inspected")
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, badKeyFile("path must name a regular file")
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		f.Close()
		return nil, badKeyFile("file mode must be exactly 0600")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		f.Close()
		return nil, badKeyFile("file ownership could not be determined")
	}
	if int(stat.Uid) != os.Getuid() {
		f.Close()
		return nil, badKeyFile("file must be owned by the user running this process")
	}
	return f, nil
}
