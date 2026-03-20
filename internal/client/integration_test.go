package client

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync-http/internal/common"
	"sync-http/internal/server"
	"testing"
	"time"
)

func TestEndToEndSync(t *testing.T) {
	upstream := t.TempDir()
	moduleRoot := filepath.Join(upstream, "mod1")
	target := t.TempDir()
	mustWrite(t, filepath.Join(moduleRoot, "keep.txt"), []byte("from-upstream"), 0o644)
	mustWrite(t, filepath.Join(moduleRoot, "tmp", "skip.txt"), []byte("skip"), 0o644)
	mustWrite(t, filepath.Join(target, "keep.txt"), []byte("old"), 0o644)
	mustWrite(t, filepath.Join(target, "remove.txt"), []byte("delete"), 0o644)

	svc, err := server.New(server.Config{
		RootDir:  upstream,
		LockDir:  filepath.Join(t.TempDir(), "locks"),
		AuditDir: filepath.Join(t.TempDir(), "audit"),
		PageSize: 2,
		Modules: map[string]server.Module{
			"mod1": {Name: "mod1", RootDir: moduleRoot, Tokens: map[string]struct{}{"tkn": {}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(svc.Handler())
	defer ts.Close()

	res, err := Run(context.Background(), Config{
		ServerURL:           ts.URL,
		Token:               "tkn",
		Module:              "mod1",
		SourcePath:          ".",
		TargetDir:           target,
		ClientID:            "test",
		ExcludeGlobs:        []string{"tmp/**"},
		DeleteGuardRatio:    1.0,
		DeleteGuardMinFiles: 1,
		PageSize:            2,
		Backoffs:            []time.Duration{10 * time.Millisecond, 20 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Downloaded == 0 {
		t.Fatal("expected downloads")
	}
	if _, err := os.Stat(filepath.Join(target, "remove.txt")); !os.IsNotExist(err) {
		t.Fatal("expected remove.txt deleted")
	}
	b, err := os.ReadFile(filepath.Join(target, "keep.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "from-upstream" {
		t.Fatalf("unexpected keep.txt: %s", string(b))
	}
	if _, err := os.Stat(filepath.Join(target, "tmp", "skip.txt")); !os.IsNotExist(err) {
		t.Fatal("excluded file should not be synced")
	}
}

func TestLockedThenWaitWebSocket(t *testing.T) {
	upstream := t.TempDir()
	moduleRoot := filepath.Join(upstream, "mod1")
	target := t.TempDir()
	mustWrite(t, filepath.Join(moduleRoot, "a.txt"), []byte("ok"), 0o644)

	svc, err := server.New(server.Config{
		RootDir:  upstream,
		LockDir:  filepath.Join(t.TempDir(), "locks"),
		AuditDir: filepath.Join(t.TempDir(), "audit"),
		Modules: map[string]server.Module{
			"mod1": {Name: "mod1", RootDir: moduleRoot, Tokens: map[string]struct{}{"tkn": {}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(svc.Handler())
	defer ts.Close()

	lockReq := map[string]any{"module": "mod1", "owner": "upstream", "ttl_seconds": 60}
	body, _ := json.Marshal(lockReq)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/locks", bytesReader(body))
	req.Header.Set("Authorization", "Bearer tkn")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("lock failed: %d", resp.StatusCode)
	}

	go func() {
		time.Sleep(120 * time.Millisecond)
		unlockBody, _ := json.Marshal(map[string]any{"module": "mod1", "owner": "upstream"})
		ur, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/locks/release", bytesReader(unlockBody))
		ur.Header.Set("Authorization", "Bearer tkn")
		ur.Header.Set("Content-Type", "application/json")
		uResp, _ := http.DefaultClient.Do(ur)
		if uResp != nil {
			uResp.Body.Close()
		}
	}()

	_, err = Run(context.Background(), Config{
		ServerURL:           ts.URL,
		Token:               "tkn",
		Module:              "mod1",
		SourcePath:          ".",
		TargetDir:           target,
		ClientID:            "test",
		DeleteGuardRatio:    1.0,
		DeleteGuardMinFiles: 1,
		Backoffs:            []time.Duration{5 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDeleteGuardAbort(t *testing.T) {
	upstream := t.TempDir()
	moduleRoot := filepath.Join(upstream, "mod1")
	target := t.TempDir()
	if err := os.MkdirAll(moduleRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		mustWrite(t, filepath.Join(target, "f", strconv.Itoa(i)+".txt"), []byte("x"), 0o644)
	}

	svc, err := server.New(server.Config{
		RootDir:  upstream,
		LockDir:  filepath.Join(t.TempDir(), "locks"),
		AuditDir: filepath.Join(t.TempDir(), "audit"),
		Modules: map[string]server.Module{
			"mod1": {Name: "mod1", RootDir: moduleRoot, Tokens: map[string]struct{}{"tkn": {}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(svc.Handler())
	defer ts.Close()

	_, err = Run(context.Background(), Config{
		ServerURL:           ts.URL,
		Token:               "tkn",
		Module:              "mod1",
		SourcePath:          ".",
		TargetDir:           target,
		ClientID:            "test",
		DeleteGuardRatio:    0.1,
		DeleteGuardMinFiles: 1,
		Backoffs:            []time.Duration{10 * time.Millisecond},
	})
	if err == nil {
		t.Fatal("expected delete guard error")
	}
}

func TestRangeResumeDownload(t *testing.T) {
	upstream := t.TempDir()
	moduleRoot := filepath.Join(upstream, "mod1")
	target := t.TempDir()
	content := []byte("abcdefghijklmnopqrstuvwxyz0123456789")
	mustWrite(t, filepath.Join(moduleRoot, "big.bin"), content, 0o644)

	svc, err := server.New(server.Config{
		RootDir:  upstream,
		LockDir:  filepath.Join(t.TempDir(), "locks"),
		AuditDir: filepath.Join(t.TempDir(), "audit"),
		Modules: map[string]server.Module{
			"mod1": {Name: "mod1", RootDir: moduleRoot, Tokens: map[string]struct{}{"tkn": {}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(svc.Handler())
	defer ts.Close()

	hc := &http.Client{}
	create := common.SessionCreateRequest{Module: "mod1", SourcePath: ".", ClientID: "t"}
	cbody, _ := json.Marshal(create)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/sessions", bytesReader(cbody))
	req.Header.Set("Authorization", "Bearer tkn")
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var sresp common.SessionCreateResponse
	if err := json.NewDecoder(resp.Body).Decode(&sresp); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()

	mreq, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/snapshots/"+sresp.SnapshotID+"/manifest", nil)
	mreq.Header.Set("Authorization", "Bearer tkn")
	mresp, err := hc.Do(mreq)
	if err != nil {
		t.Fatal(err)
	}
	var mp common.ManifestPageResponse
	if err := json.NewDecoder(mresp.Body).Decode(&mp); err != nil {
		mresp.Body.Close()
		t.Fatal(err)
	}
	mresp.Body.Close()
	if len(mp.Entries) != 1 {
		t.Fatalf("expected one entry, got %d", len(mp.Entries))
	}
	e := mp.Entries[0]

	staging := filepath.Join(target, ".sync-http-staging", sresp.SessionID)
	part := filepath.Join(staging, filepath.FromSlash(e.Path)+".part")
	if err := os.MkdirAll(filepath.Dir(part), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(part, content[:10], 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = downloadAndPromote(context.Background(), hc, Config{ServerURL: ts.URL, Token: "tkn", Module: "mod1", TargetDir: target}, sresp.SnapshotID, e, staging)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(target, "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != string(content) {
		t.Fatalf("resume mismatch: %q", string(b))
	}
}

func mustWrite(t *testing.T, path string, data []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
}

func bytesReader(b []byte) *bytes.Reader {
	return bytes.NewReader(b)
}
