package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type AuditWriter struct {
	dir           string
	retentionDays int
	mu            sync.Mutex
}

func NewAuditWriter(dir string, retentionDays int) (*AuditWriter, error) {
	if retentionDays <= 0 {
		retentionDays = 30
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	a := &AuditWriter{dir: dir, retentionDays: retentionDays}
	a.cleanup()
	return a, nil
}

func (a *AuditWriter) Write(event map[string]any) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	event["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
	b, err := json.Marshal(event)
	if err != nil {
		return
	}
	name := "audit-" + time.Now().UTC().Format("2006-01-02") + ".jsonl"
	path := filepath.Join(a.dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = f.Write(append(b, '\n'))
	_ = f.Close()
}

func (a *AuditWriter) cleanup() {
	ents, err := os.ReadDir(a.dir)
	if err != nil {
		return
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -a.retentionDays)
	for _, ent := range ents {
		if ent.IsDir() {
			continue
		}
		name := ent.Name()
		if !strings.HasPrefix(name, "audit-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		datePart := strings.TrimSuffix(strings.TrimPrefix(name, "audit-"), ".jsonl")
		d, err := time.Parse("2006-01-02", datePart)
		if err != nil {
			continue
		}
		if d.Before(cutoff) {
			_ = os.Remove(filepath.Join(a.dir, name))
		}
	}
}
