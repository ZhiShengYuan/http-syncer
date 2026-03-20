package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync-http/internal/common"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func testServiceWithModules(t *testing.T) (*Service, string) {
	t.Helper()
	root := t.TempDir()
	mod1 := filepath.Join(root, "mod1")
	mod2 := filepath.Join(root, "mod2")
	if err := os.MkdirAll(mod1, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(mod2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mod1, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mod2, "b.txt"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc, err := New(Config{
		RootDir:  root,
		LockDir:  t.TempDir(),
		AuditDir: t.TempDir(),
		Modules: map[string]Module{
			"mod1": {Name: "mod1", RootDir: mod1, Tokens: map[string]struct{}{"tkn1": {}}},
			"mod2": {Name: "mod2", RootDir: mod2, Tokens: map[string]struct{}{"tkn2": {}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc, root
}

func TestLockWaitWebSocketNotifiesUnlock(t *testing.T) {
	root := t.TempDir()
	modRoot := root
	svc, err := New(Config{
		RootDir:  root,
		LockDir:  t.TempDir(),
		AuditDir: t.TempDir(),
		Modules: map[string]Module{
			"mod1": {Name: "mod1", RootDir: modRoot, Tokens: map[string]struct{}{"tkn": {}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(svc.Handler())
	defer ts.Close()

	lockBody, _ := json.Marshal(map[string]any{"module": "mod1", "owner": "upstream", "ttl_seconds": 60})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/locks", bytes.NewReader(lockBody))
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

	wsURL := strings.Replace(ts.URL, "http://", "ws://", 1) + "/v1/locks/wait/ws?module=mod1"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Authorization": []string{"Bearer tkn"}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	go func() {
		time.Sleep(50 * time.Millisecond)
		unlockBody, _ := json.Marshal(map[string]any{"module": "mod1", "owner": "upstream"})
		ureq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/locks/release", bytes.NewReader(unlockBody))
		ureq.Header.Set("Authorization", "Bearer tkn")
		ureq.Header.Set("Content-Type", "application/json")
		uResp, _ := http.DefaultClient.Do(ureq)
		if uResp != nil {
			uResp.Body.Close()
		}
	}()

	type lockEvent struct {
		Event string `json:"event"`
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var ev lockEvent
		if err := conn.ReadJSON(&ev); err != nil {
			t.Fatal(err)
		}
		if ev.Event == "unlocked" {
			return
		}
	}
	t.Fatal("timed out waiting for unlocked event")
}

func TestModuleAuthEnforced(t *testing.T) {
	svc, _ := testServiceWithModules(t)
	ts := httptest.NewServer(svc.Handler())
	defer ts.Close()
	body, _ := json.Marshal(common.SessionCreateRequest{Module: "mod1", SourcePath: ".", ClientID: "c"})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/sessions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer tkn2")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestSessionRequiresModule(t *testing.T) {
	svc, _ := testServiceWithModules(t)
	ts := httptest.NewServer(svc.Handler())
	defer ts.Close()
	body, _ := json.Marshal(common.SessionCreateRequest{SourcePath: ".", ClientID: "c"})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/sessions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer tkn1")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestModuleLockIsolation(t *testing.T) {
	svc, _ := testServiceWithModules(t)
	ts := httptest.NewServer(svc.Handler())
	defer ts.Close()
	lockBody, _ := json.Marshal(map[string]any{"module": "mod1", "owner": "upstream", "ttl_seconds": 60})
	lockReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/locks", bytes.NewReader(lockBody))
	lockReq.Header.Set("Authorization", "Bearer tkn1")
	lockReq.Header.Set("Content-Type", "application/json")
	lockResp, err := http.DefaultClient.Do(lockReq)
	if err != nil {
		t.Fatal(err)
	}
	lockResp.Body.Close()
	if lockResp.StatusCode != http.StatusNoContent {
		t.Fatalf("lock failed: %d", lockResp.StatusCode)
	}

	body2, _ := json.Marshal(common.SessionCreateRequest{Module: "mod2", SourcePath: ".", ClientID: "c"})
	req2, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/sessions", bytes.NewReader(body2))
	req2.Header.Set("Authorization", "Bearer tkn2")
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected mod2 unaffected, got %d", resp2.StatusCode)
	}
}
