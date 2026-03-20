package server

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync-http/internal/common"
	"sync-http/internal/filter"
	"time"
)

type Snapshot struct {
	ID         string
	SourceDir  string
	Entries    []common.ManifestEntry
	ByPath     map[string]common.ManifestEntry
	ByChecksum map[string]common.ManifestEntry
	CreatedAt  time.Time
}

func BuildSnapshot(id string, sourceDir string, ex *filter.Excluder) (*Snapshot, error) {
	entries := make([]common.ManifestEntry, 0, 1024)
	byPath := make(map[string]common.ManifestEntry)
	byChecksum := make(map[string]common.ManifestEntry)

	err := filepath.WalkDir(sourceDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == sourceDir {
			return nil
		}
		rel, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if ok, _ := ex.Match(rel); ok {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := info.Mode()
		if mode.IsRegular() {
			sum, err := checksum(path)
			if err != nil {
				return err
			}
			entry := common.ManifestEntry{
				Path:     rel,
				Type:     "file",
				Size:     info.Size(),
				Mode:     uint32(mode.Perm()),
				Mtime:    info.ModTime().Unix(),
				Checksum: sum,
			}
			entries = append(entries, entry)
			byPath[rel] = entry
			if _, ok := byChecksum[entry.Checksum]; !ok {
				byChecksum[entry.Checksum] = entry
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Path < entries[j].Path
	})

	return &Snapshot{ID: id, SourceDir: sourceDir, Entries: entries, ByPath: byPath, ByChecksum: byChecksum, CreatedAt: time.Now()}, nil
}

func checksum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
