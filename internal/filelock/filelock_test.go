package filelock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSecondLockFailsUntilUnlocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.lock")
	open := func() *os.File {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		return f
	}
	a, b := open(), open()
	if err := TryLock(a); err != nil {
		t.Fatal(err)
	}
	if err := TryLock(b); !errors.Is(err, ErrLocked) {
		t.Fatalf("second lock: %v", err)
	}
	if err := Unlock(a); err != nil {
		t.Fatal(err)
	}
	if err := TryLock(b); err != nil {
		t.Fatalf("after unlock: %v", err)
	}
}
