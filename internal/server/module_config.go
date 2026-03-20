package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type FileConfig struct {
	Server struct {
		RootDir            string `yaml:"root_dir"`
		LockDir            string `yaml:"lock_dir"`
		AuditDir           string `yaml:"audit_dir"`
		AuditRetentionDays int    `yaml:"audit_retention_days"`
		PageSize           int    `yaml:"page_size"`
		LockTTL            string `yaml:"lock_ttl"`
		SnapshotTTL        string `yaml:"snapshot_ttl"`
	} `yaml:"server"`
	Modules []ModuleYAML `yaml:"modules"`
}

type ModuleYAML struct {
	Name   string   `yaml:"name"`
	Root   string   `yaml:"root"`
	Tokens []string `yaml:"tokens"`
}

type Module struct {
	Name    string
	RootDir string
	Tokens  map[string]struct{}
}

func LoadFileConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var fc FileConfig
	if err := yaml.Unmarshal(b, &fc); err != nil {
		return nil, err
	}
	if fc.Server.RootDir == "" {
		return nil, fmt.Errorf("server.root_dir is required")
	}
	rootDir, err := filepath.Abs(fc.Server.RootDir)
	if err != nil {
		return nil, err
	}

	lockDir := fc.Server.LockDir
	if lockDir == "" {
		lockDir = ".sync-locks"
	}
	auditDir := fc.Server.AuditDir
	if auditDir == "" {
		auditDir = "./audit"
	}
	retention := fc.Server.AuditRetentionDays
	if retention <= 0 {
		retention = 30
	}
	pageSize := fc.Server.PageSize
	if pageSize <= 0 {
		pageSize = 5000
	}
	lockTTL := 10 * time.Minute
	if strings.TrimSpace(fc.Server.LockTTL) != "" {
		d, err := time.ParseDuration(fc.Server.LockTTL)
		if err != nil {
			return nil, fmt.Errorf("parse lock_ttl: %w", err)
		}
		lockTTL = d
	}
	snapshotTTL := 30 * time.Minute
	if strings.TrimSpace(fc.Server.SnapshotTTL) != "" {
		d, err := time.ParseDuration(fc.Server.SnapshotTTL)
		if err != nil {
			return nil, fmt.Errorf("parse snapshot_ttl: %w", err)
		}
		snapshotTTL = d
	}

	mods := map[string]Module{}
	for _, m := range fc.Modules {
		if strings.TrimSpace(m.Name) == "" {
			return nil, fmt.Errorf("module name is required")
		}
		if _, exists := mods[m.Name]; exists {
			return nil, fmt.Errorf("duplicate module name %q", m.Name)
		}
		if strings.TrimSpace(m.Root) == "" {
			return nil, fmt.Errorf("module %q root is required", m.Name)
		}
		absRoot := m.Root
		if !filepath.IsAbs(absRoot) {
			absRoot = filepath.Join(rootDir, m.Root)
		}
		absRoot, err = filepath.Abs(absRoot)
		if err != nil {
			return nil, err
		}
		rel, err := filepath.Rel(rootDir, absRoot)
		if err != nil || strings.HasPrefix(rel, "..") {
			return nil, fmt.Errorf("module %q root must be under server.root_dir", m.Name)
		}
		tokens := map[string]struct{}{}
		for _, tok := range m.Tokens {
			if strings.TrimSpace(tok) == "" {
				continue
			}
			tokens[tok] = struct{}{}
		}
		if len(tokens) == 0 {
			return nil, fmt.Errorf("module %q must define at least one token", m.Name)
		}
		mods[m.Name] = Module{Name: m.Name, RootDir: absRoot, Tokens: tokens}
	}
	if len(mods) == 0 {
		return nil, fmt.Errorf("at least one module is required")
	}

	return &Config{
		RootDir:            rootDir,
		LockDir:            lockDir,
		LockTTL:            lockTTL,
		PageSize:           pageSize,
		SnapshotTTL:        snapshotTTL,
		Modules:            mods,
		AuditDir:           auditDir,
		AuditRetentionDays: retention,
	}, nil
}
