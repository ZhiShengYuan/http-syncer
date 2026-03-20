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
	if cfg.ListenAddr != "0.0.0.0" || cfg.ListenPort != 8080 {
		t.Fatalf("unexpected listen defaults: %s:%d", cfg.ListenAddr, cfg.ListenPort)
	}
	if cfg.ProcessLogPath != "./process.log" {
		t.Fatalf("unexpected process log default: %s", cfg.ProcessLogPath)
	}
}

func TestLoadFileConfigListenAndTrustedProxy(t *testing.T) {
	root := t.TempDir()
	modDir := filepath.Join(root, "mod1")
	if err := os.MkdirAll(modDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(t.TempDir(), "server.yaml")
	content := "server:\n  root_dir: " + root + "\n  listen_addr: 127.0.0.1\n  listen_port: 9090\n  debug: true\n  process_log: /tmp/sync-http/process.log\n  trusted_proxies:\n    - 127.0.0.1\n    - 10.0.0.0/8\nmodules:\n  - name: mod1\n    root: mod1\n    tokens:\n      - tkn\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadFileConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ListenAddr != "127.0.0.1" || cfg.ListenPort != 9090 {
		t.Fatalf("unexpected listen config: %s:%d", cfg.ListenAddr, cfg.ListenPort)
	}
	if len(cfg.TrustedProxies) != 2 {
		t.Fatalf("unexpected trusted proxies: %#v", cfg.TrustedProxies)
	}
	if !cfg.Debug {
		t.Fatal("expected debug enabled")
	}
	if cfg.ProcessLogPath != "/tmp/sync-http/process.log" {
		t.Fatalf("unexpected process log path: %s", cfg.ProcessLogPath)
	}
}
