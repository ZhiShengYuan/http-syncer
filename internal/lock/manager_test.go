package lock

import (
	"errors"
	"testing"
	"time"
)

func TestTryAcquireAndRelease(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.TryAcquire("repo/a", "upstream", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := m.TryAcquire("repo/a", "other", time.Minute); !errors.Is(err, ErrLocked) {
		t.Fatalf("expected locked error, got %v", err)
	}
	locked, owner := m.IsLocked("repo/a")
	if !locked || owner != "upstream" {
		t.Fatalf("unexpected lock state: locked=%v owner=%s", locked, owner)
	}
	m.Release("repo/a", "upstream")
	locked, _ = m.IsLocked("repo/a")
	if locked {
		t.Fatal("expected unlocked")
	}
}
