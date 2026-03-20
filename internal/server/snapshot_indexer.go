package server

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync-http/internal/common"
	"sync-http/internal/filter"
	"time"

	"github.com/fsnotify/fsnotify"
)

type snapshotIndexer struct {
	modules map[string]*moduleIndex
	plog    *processLogger
}

type moduleIndex struct {
	name    string
	rootDir string
	plog    *processLogger

	mu      sync.RWMutex
	data    *indexedData
	lastErr error
}

type indexedData struct {
	entries   []common.ManifestEntry
	byPath    map[string]common.ManifestEntry
	updatedAt time.Time
}

func newSnapshotIndexer(modules map[string]Module, plog *processLogger) (*snapshotIndexer, error) {
	idx := &snapshotIndexer{modules: make(map[string]*moduleIndex, len(modules)), plog: plog}
	for name, mod := range modules {
		mi := &moduleIndex{name: name, rootDir: mod.RootDir, plog: plog}
		if err := mi.rebuild(); err != nil {
			return nil, err
		}
		idx.modules[name] = mi
		go mi.watch()
	}
	return idx, nil
}

func (s *snapshotIndexer) buildSnapshot(snapshotID, module, sourcePath string, ex *filter.Excluder) (*Snapshot, error) {
	mi := s.modules[module]
	if mi == nil {
		return nil, ErrUnknownModule
	}
	return mi.buildSnapshot(snapshotID, sourcePath, ex)
}

func (m *moduleIndex) buildSnapshot(snapshotID, sourcePath string, ex *filter.Excluder) (*Snapshot, error) {
	m.mu.RLock()
	data := m.data
	lastErr := m.lastErr
	m.mu.RUnlock()
	if data == nil {
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, ErrSnapshotNotReady
	}

	sourcePath = filepath.Clean(sourcePath)
	if sourcePath == "." || sourcePath == "" {
		sourcePath = "."
	}
	prefix := ""
	if sourcePath != "." {
		prefix = filepath.ToSlash(sourcePath)
		prefix = strings.TrimPrefix(prefix, "./")
		prefix = strings.TrimPrefix(prefix, "/")
	}

	entries := make([]common.ManifestEntry, 0, len(data.entries))
	byPath := make(map[string]common.ManifestEntry)
	for _, item := range data.entries {
		rel := item.Path
		if prefix != "" {
			if rel == prefix {
				rel = "."
			} else if strings.HasPrefix(rel, prefix+"/") {
				rel = strings.TrimPrefix(rel, prefix+"/")
			} else {
				continue
			}
		}
		if rel == "." {
			continue
		}
		if ok, _ := ex.Match(rel); ok {
			continue
		}
		entry := item
		entry.Path = rel
		entries = append(entries, entry)
		byPath[rel] = entry
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })

	return &Snapshot{
		ID:        snapshotID,
		SourceDir: filepath.Join(m.rootDir, filepath.FromSlash(prefix)),
		Entries:   entries,
		ByPath:    byPath,
		CreatedAt: time.Now(),
	}, nil
}

func (m *moduleIndex) watch() {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		m.plog.Debugf("snapshot watch init failed module=%s err=%v", m.name, err)
		return
	}
	defer watcher.Close()

	if err := addRecursiveWatches(watcher, m.rootDir); err != nil {
		m.plog.Debugf("snapshot watch add failed module=%s err=%v", m.name, err)
		return
	}

	debounce := time.NewTimer(time.Hour)
	if !debounce.Stop() {
		<-debounce.C
	}
	pending := false
	for {
		select {
		case ev, ok := <-watcher.Events:
			if !ok {
				return
			}
			if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename|fsnotify.Chmod) == 0 {
				continue
			}
			if ev.Op&fsnotify.Create != 0 {
				_ = addRecursiveWatches(watcher, ev.Name)
			}
			if !pending {
				pending = true
				debounce.Reset(1200 * time.Millisecond)
			}
		case <-debounce.C:
			if err := m.rebuild(); err != nil {
				m.plog.Debugf("snapshot rebuild failed module=%s err=%v", m.name, err)
			}
			pending = false
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			m.plog.Debugf("snapshot watcher error module=%s err=%v", m.name, err)
		}
	}
}

func (m *moduleIndex) rebuild() error {
	start := time.Now()
	entries, byPath, err := scanModuleSnapshot(m.rootDir)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		m.lastErr = err
		return err
	}
	m.lastErr = nil
	m.data = &indexedData{entries: entries, byPath: byPath, updatedAt: time.Now()}
	m.plog.Infof("snapshot index rebuilt module=%s files=%d cost_ms=%d", m.name, len(entries), time.Since(start).Milliseconds())
	return nil
}

func scanModuleSnapshot(root string) ([]common.ManifestEntry, map[string]common.ManifestEntry, error) {
	entries := make([]common.ManifestEntry, 0, 1024)
	byPath := make(map[string]common.ManifestEntry)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		sum, err := checksum(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		entry := common.ManifestEntry{
			Path:     rel,
			Type:     "file",
			Size:     info.Size(),
			Mode:     uint32(info.Mode().Perm()),
			Mtime:    info.ModTime().Unix(),
			Checksum: sum,
		}
		entries = append(entries, entry)
		byPath[rel] = entry
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, byPath, nil
}

func addRecursiveWatches(w *fsnotify.Watcher, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return nil
	}
	return filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			_ = w.Add(p)
		}
		return nil
	})
}

var (
	ErrUnknownModule    = errors.New("unknown module")
	ErrSnapshotNotReady = errors.New("snapshot not ready")
)
