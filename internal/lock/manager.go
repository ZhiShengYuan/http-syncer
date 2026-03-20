package lock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var ErrLocked = errors.New("path is locked")

type Manager struct {
	dir string
	mu  sync.Mutex
	mem map[string]lockRecord
	now func() time.Time
}

type lockRecord struct {
	path      string
	owner     string
	expiresAt time.Time
}

func NewManager(dir string) (*Manager, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Manager{dir: dir, mem: map[string]lockRecord{}, now: time.Now}, nil
}

func normalizePath(path string) string {
	clean := filepath.ToSlash(filepath.Clean(path))
	clean = strings.TrimPrefix(clean, "./")
	if clean == "." {
		return "root"
	}
	return strings.ReplaceAll(clean, "/", "_")
}

func (m *Manager) lockFile(path string) string {
	return filepath.Join(m.dir, normalizePath(path)+".lock")
}

func cleanPath(path string) string {
	clean := filepath.ToSlash(filepath.Clean(path))
	clean = strings.TrimPrefix(clean, "./")
	if clean == "" {
		return "."
	}
	return clean
}

func overlaps(a, b string) bool {
	a = cleanPath(a)
	b = cleanPath(b)
	if a == "." || b == "." {
		return true
	}
	if a == b {
		return true
	}
	if strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/") {
		return true
	}
	return false
}

func parseLockFile(path string) (owner string, exp time.Time, lockPath string, ok bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", time.Time{}, "", false
	}
	parts := strings.Split(strings.TrimSpace(string(b)), "|")
	if len(parts) != 3 {
		return "", time.Time{}, "", false
	}
	exp, err = time.Parse(time.RFC3339Nano, parts[1])
	if err != nil {
		return "", time.Time{}, "", false
	}
	return parts[0], exp, cleanPath(parts[2]), true
}

func (m *Manager) TryAcquire(path string, owner string, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	path = cleanPath(path)

	now := m.now()
	for p, rec := range m.mem {
		if rec.expiresAt.Before(now) {
			delete(m.mem, p)
			continue
		}
		if overlaps(path, p) {
			return fmt.Errorf("%w by %s", ErrLocked, rec.owner)
		}
	}

	ents, err := os.ReadDir(m.dir)
	if err == nil {
		for _, ent := range ents {
			if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".lock") {
				continue
			}
			lf := filepath.Join(m.dir, ent.Name())
			owner2, exp, lockedPath, ok := parseLockFile(lf)
			if !ok {
				_ = os.Remove(lf)
				continue
			}
			if exp.Before(now) {
				_ = os.Remove(lf)
				continue
			}
			if overlaps(path, lockedPath) {
				return fmt.Errorf("%w by %s", ErrLocked, owner2)
			}
		}
	}

	exp := now.Add(ttl)
	m.mem[path] = lockRecord{path: path, owner: owner, expiresAt: exp}
	content := owner + "|" + exp.Format(time.RFC3339Nano) + "|" + path
	lf := m.lockFile(path)
	if err := os.WriteFile(lf, []byte(content), 0o644); err != nil {
		delete(m.mem, path)
		return err
	}
	return nil
}

func (m *Manager) Release(path string, owner string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	path = cleanPath(path)
	if rec, ok := m.mem[path]; ok {
		if owner == "" || rec.owner == owner {
			delete(m.mem, path)
		}
	}
	_ = os.Remove(m.lockFile(path))
}

func (m *Manager) IsLocked(path string) (bool, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	path = cleanPath(path)
	now := m.now()
	for p, rec := range m.mem {
		if rec.expiresAt.Before(now) {
			delete(m.mem, p)
			continue
		}
		if overlaps(path, rec.path) {
			return true, rec.owner
		}
	}
	ents, err := os.ReadDir(m.dir)
	if err != nil {
		return false, ""
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
	for _, ent := range ents {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".lock") {
			continue
		}
		lf := filepath.Join(m.dir, ent.Name())
		owner, exp, lockedPath, ok := parseLockFile(lf)
		if !ok {
			_ = os.Remove(lf)
			continue
		}
		if exp.Before(now) {
			_ = os.Remove(lf)
			continue
		}
		if overlaps(path, lockedPath) {
			return true, owner
		}
	}
	return false, ""
}
