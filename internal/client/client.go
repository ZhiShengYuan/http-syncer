package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gorilla/websocket"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync-http/internal/common"
	"sync-http/internal/filter"
	"time"
)

type Config struct {
	ServerURL           string
	Token               string
	UserAgent           string
	Module              string
	SourcePath          string
	TargetDir           string
	ClientID            string
	Debug               bool
	ProcessLogPath      string
	ExcludeGlobs        []string
	ExcludeRegex        []string
	DeleteGuardRatio    float64
	DeleteGuardMinFiles int
	ForceDeleteGuard    bool
	DryRun              bool
	PageSize            int
	Backoffs            []time.Duration
	plog                *processLogger
}

const defaultUserAgent = "go-sync/1.0"

type Result struct {
	Downloaded int
	Deleted    int
	Bytes      int64
	SessionID  string
	SnapshotID string
}

func Run(ctx context.Context, cfg Config) (*Result, error) {
	if strings.TrimSpace(cfg.UserAgent) == "" {
		cfg.UserAgent = defaultUserAgent
	}
	plog, err := newProcessLogger(cfg.ProcessLogPath, cfg.Debug)
	if err != nil {
		return nil, err
	}
	cfg.plog = plog
	cfg.plog.Infof("sync start server=%s module=%s source=%s target=%s dry_run=%t", cfg.ServerURL, cfg.Module, cfg.SourcePath, cfg.TargetDir, cfg.DryRun)

	if len(cfg.Backoffs) == 0 {
		cfg.Backoffs = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}
	}
	if cfg.PageSize <= 0 {
		cfg.PageSize = 5000
	}

	ex, err := filter.NewExcluder(cfg.ExcludeGlobs, cfg.ExcludeRegex)
	if err != nil {
		return nil, err
	}

	hc := &http.Client{Timeout: 0}
	createReq := common.SessionCreateRequest{
		Module:        cfg.Module,
		SourcePath:    cfg.SourcePath,
		ExcludeGlobs:  cfg.ExcludeGlobs,
		ExcludeRegex:  cfg.ExcludeRegex,
		ClientID:      cfg.ClientID,
		RequestedPage: cfg.PageSize,
	}
	if createReq.SourcePath == "" {
		createReq.SourcePath = "."
	}
	if strings.TrimSpace(createReq.Module) == "" {
		return nil, fmt.Errorf("module is required")
	}
	createBody, _ := json.Marshal(createReq)

	var sresp common.SessionCreateResponse
	transientRetry := 0
	for {
		locked, err := createSessionOnce(ctx, hc, cfg, createBody, &sresp)
		if err == nil {
			cfg.plog.Infof("session created session=%s snapshot=%s", sresp.SessionID, sresp.SnapshotID)
			break
		}
		if locked {
			cfg.plog.Infof("module locked, waiting websocket module=%s", createReq.Module)
			if err := waitForUnlockWS(ctx, cfg, createReq.Module); err != nil {
				return nil, err
			}
			cfg.plog.Infof("module unlocked notification module=%s", createReq.Module)
			transientRetry = 0
			continue
		}
		if transientRetry >= len(cfg.Backoffs) {
			cfg.plog.Debugf("session create final error: %v", err)
			return nil, err
		}
		cfg.plog.Debugf("session create transient retry=%d err=%v", transientRetry, err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(cfg.Backoffs[transientRetry]):
		}
		transientRetry++
	}

	remote, err := fetchManifest(ctx, hc, cfg, sresp.SnapshotID)
	if err != nil {
		return nil, err
	}
	cfg.plog.Infof("manifest files=%d", len(remote))
	local, err := scanLocal(cfg.TargetDir, ex)
	if err != nil {
		return nil, err
	}
	cfg.plog.Infof("local files=%d", len(local))

	toDownload := make([]common.ManifestEntry, 0)
	for p, re := range remote {
		le, ok := local[p]
		if !ok || le.Size != re.Size || le.Mtime != re.Mtime || le.Mode != re.Mode {
			toDownload = append(toDownload, re)
		}
	}
	sort.Slice(toDownload, func(i, j int) bool { return toDownload[i].Path < toDownload[j].Path })

	toDelete := make([]string, 0)
	for p := range local {
		if _, ok := remote[p]; !ok {
			toDelete = append(toDelete, p)
		}
	}
	sort.Strings(toDelete)
	cfg.plog.Infof("plan download=%d delete=%d", len(toDownload), len(toDelete))

	if err := enforceDeleteGuard(cfg, len(local), len(toDelete)); err != nil {
		cfg.plog.Debugf("delete guard error: %v", err)
		return nil, err
	}

	res := &Result{SessionID: sresp.SessionID, SnapshotID: sresp.SnapshotID}
	staging := filepath.Join(cfg.TargetDir, ".sync-http-staging", sresp.SessionID)
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return nil, err
	}

	for _, e := range toDownload {
		if cfg.DryRun {
			cfg.plog.Debugf("dry run skip download path=%s", e.Path)
			continue
		}
		cfg.plog.Debugf("download path=%s", e.Path)
		written, err := downloadAndPromote(ctx, hc, cfg, sresp.SnapshotID, e, staging)
		if err != nil {
			return nil, err
		}
		res.Downloaded++
		res.Bytes += written
	}

	for _, p := range toDelete {
		if cfg.DryRun {
			cfg.plog.Debugf("dry run skip delete path=%s", p)
			continue
		}
		if err := os.Remove(filepath.Join(cfg.TargetDir, filepath.FromSlash(p))); err == nil {
			res.Deleted++
			cfg.plog.Debugf("deleted path=%s", p)
		}
	}

	if !cfg.DryRun {
		_ = os.RemoveAll(staging)
	}

	if err := commitSession(ctx, hc, cfg, *res); err != nil {
		return nil, err
	}
	cfg.plog.Infof("sync complete downloaded=%d deleted=%d bytes=%d", res.Downloaded, res.Deleted, res.Bytes)
	return res, nil
}

func createSessionOnce(ctx context.Context, hc *http.Client, cfg Config, createBody []byte, out *common.SessionCreateResponse) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(cfg.ServerURL, "/")+"/v1/sessions", bytes.NewReader(createBody))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	req.Header.Set("User-Agent", cfg.UserAgent)
	resp, err := hc.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusLocked {
		return true, errors.New("upstream locked")
	}
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("create session failed: %s", string(b))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return false, err
	}
	return false, nil
}

func waitForUnlockWS(ctx context.Context, cfg Config, module string) error {
	base := strings.TrimSuffix(cfg.ServerURL, "/")
	wsURL := strings.Replace(base, "http://", "ws://", 1)
	wsURL = strings.Replace(wsURL, "https://", "wss://", 1)
	vals := url.Values{}
	vals.Set("module", module)
	wsURL = wsURL + "/v1/locks/wait/ws?" + vals.Encode()

	dialer := websocket.Dialer{}
	headers := http.Header{}
	if cfg.Token != "" {
		headers.Set("Authorization", "Bearer "+cfg.Token)
	}
	headers.Set("User-Agent", cfg.UserAgent)
	conn, _, err := dialer.DialContext(ctx, wsURL, headers)
	if err != nil {
		return err
	}
	defer conn.Close()

	type lockEvent struct {
		Event string `json:"event"`
	}
	for {
		var ev lockEvent
		if err := conn.ReadJSON(&ev); err != nil {
			return err
		}
		cfg.plog.Debugf("websocket event=%s module=%s", ev.Event, module)
		if ev.Event == "unlocked" {
			return nil
		}
	}
}

func fetchManifest(ctx context.Context, hc *http.Client, cfg Config, snapshotID string) (map[string]common.ManifestEntry, error) {
	out := map[string]common.ManifestEntry{}
	cursor := ""
	for {
		u := strings.TrimSuffix(cfg.ServerURL, "/") + "/v1/snapshots/" + snapshotID + "/manifest?page_size=" + strconv.Itoa(cfg.PageSize)
		if cursor != "" {
			u += "&cursor=" + cursor
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		if cfg.Token != "" {
			req.Header.Set("Authorization", "Bearer "+cfg.Token)
		}
		req.Header.Set("User-Agent", cfg.UserAgent)
		resp, err := hc.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode >= 300 {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return nil, fmt.Errorf("manifest failed: %s", string(b))
		}
		var page common.ManifestPageResponse
		if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
			resp.Body.Close()
			return nil, err
		}
		resp.Body.Close()
		for _, e := range page.Entries {
			out[e.Path] = e
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return out, nil
}

func scanLocal(root string, ex *filter.Excluder) (map[string]common.ManifestEntry, error) {
	res := map[string]common.ManifestEntry{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, ".sync-http-staging") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if ok, _ := ex.Match(rel); ok {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		res[rel] = common.ManifestEntry{Path: rel, Size: info.Size(), Mtime: info.ModTime().Unix(), Mode: uint32(info.Mode().Perm())}
		return nil
	})
	if os.IsNotExist(err) {
		if mkErr := os.MkdirAll(root, 0o755); mkErr != nil {
			return nil, mkErr
		}
		return res, nil
	}
	return res, err
}

func enforceDeleteGuard(cfg Config, totalLocal int, plannedDeletes int) error {
	if cfg.ForceDeleteGuard || plannedDeletes == 0 {
		return nil
	}
	if totalLocal < cfg.DeleteGuardMinFiles {
		return nil
	}
	ratio := float64(plannedDeletes) / float64(totalLocal)
	if ratio > cfg.DeleteGuardRatio {
		return fmt.Errorf("delete guard triggered: planned=%d total=%d ratio=%.4f threshold=%.4f", plannedDeletes, totalLocal, ratio, cfg.DeleteGuardRatio)
	}
	return nil
}

func downloadAndPromote(ctx context.Context, hc *http.Client, cfg Config, snapshotID string, entry common.ManifestEntry, staging string) (int64, error) {
	stagePath := filepath.Join(staging, filepath.FromSlash(entry.Path))
	if err := os.MkdirAll(filepath.Dir(stagePath), 0o755); err != nil {
		return 0, err
	}
	part := stagePath + ".part"

	var offset int64
	if st, err := os.Stat(part); err == nil {
		offset = st.Size()
	}

	values := url.Values{}
	values.Set("snapshot_id", snapshotID)
	values.Set("path", entry.Path)
	u := strings.TrimSuffix(cfg.ServerURL, "/") + "/v1/objects?" + values.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	req.Header.Set("User-Agent", cfg.UserAgent)
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}

	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("download %s failed: %s", entry.Path, string(b))
	}

	flag := os.O_CREATE | os.O_WRONLY
	if offset > 0 && resp.StatusCode == http.StatusPartialContent {
		flag |= os.O_APPEND
	} else {
		flag |= os.O_TRUNC
		offset = 0
	}
	f, err := os.OpenFile(part, flag, 0o644)
	if err != nil {
		return 0, err
	}
	written, err := io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		return 0, err
	}

	sum, err := fileChecksum(part)
	if err != nil {
		return 0, err
	}
	if sum != entry.Checksum {
		return 0, fmt.Errorf("checksum mismatch for %s", entry.Path)
	}

	if err := os.Chmod(part, os.FileMode(entry.Mode)); err != nil {
		return 0, err
	}
	mt := time.Unix(entry.Mtime, 0)
	if err := os.Chtimes(part, mt, mt); err != nil {
		return 0, err
	}

	target := filepath.Join(cfg.TargetDir, filepath.FromSlash(entry.Path))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return 0, err
	}
	if err := os.Rename(part, target); err != nil {
		return 0, err
	}
	return written + offset, nil
}

func fileChecksum(path string) (string, error) {
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

func commitSession(ctx context.Context, hc *http.Client, cfg Config, res Result) error {
	body, _ := json.Marshal(common.SessionCommitRequest{Downloaded: res.Downloaded, Deleted: res.Deleted, Bytes: res.Bytes, Status: "ok"})
	u := strings.TrimSuffix(cfg.ServerURL, "/") + "/v1/sessions/" + res.SessionID + "/commit"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	req.Header.Set("User-Agent", cfg.UserAgent)
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("commit failed: %s", string(b))
	}
	return nil
}
