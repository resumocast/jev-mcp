//go:build unix

package typesafe

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO named where the key file belongs must be rejected, and must be
// rejected *promptly*. Opening a FIFO for reading blocks until a writer
// appears, so without O_NONBLOCK this call would hang the MCP server at
// startup with no error and no timeout. The deadline here is the assertion
// that matters; the error check alone would pass even if the open blocked
// until some unrelated writer showed up.
func TestReadKeyRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jev-pi.key")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("cannot create FIFO: %v", err)
	}
	t.Cleanup(func() { os.Remove(path) })

	done := make(chan error, 1)
	go func() {
		_, err := ReadKey(path)
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, ErrKeyFile) {
			t.Fatalf("want ErrKeyFile, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ReadKey blocked on a FIFO")
	}
}

// Character devices are not credentials either, and /dev/zero would otherwise
// supply an endless stream of bytes to read.
func TestReadKeyRejectsDevice(t *testing.T) {
	if _, err := os.Stat("/dev/zero"); err != nil {
		t.Skip("/dev/zero is not available")
	}
	// /dev/zero is not mode 0600 and is not owned by this user either, so this
	// asserts the layered checks reject it however it is configured locally.
	if _, err := ReadKey("/dev/zero"); !errors.Is(err, ErrKeyFile) {
		t.Fatalf("want ErrKeyFile, got %v", err)
	}
}
