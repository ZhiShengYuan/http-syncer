package client

import "testing"

func TestDeleteGuard(t *testing.T) {
	cfg := Config{DeleteGuardRatio: 0.1, DeleteGuardMinFiles: 10}
	if err := enforceDeleteGuard(cfg, 100, 5); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := enforceDeleteGuard(cfg, 100, 15); err == nil {
		t.Fatal("expected delete guard error")
	}
	cfg.ForceDeleteGuard = true
	if err := enforceDeleteGuard(cfg, 100, 50); err != nil {
		t.Fatalf("force should bypass guard: %v", err)
	}
}
