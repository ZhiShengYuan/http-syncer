package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadFileConfig(t *testing.T) {
	root := t.TempDir()
	modDir := filepath.Join(root, "mod1")
	if err := os.MkdirAll(modDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(t.TempDir(), "server.yaml")
	content := "server:\n  root_dir: " + root + "\n  lock_dir: " + filepath.Join(t.TempDir(), "locks") + "\n  audit_dir: " + filepath.Join(t.TempDir(), "audit") + "\n  audit_retention_days: 30\nmodules:\n  - name: mod1\n    root: mod1\n    tokens:\n      - tkn\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFileConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Modules["mod1"]; !ok {
		t.Fatal("expected module mod1")
	}
}
