package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync-http/internal/common"
	"sync-http/internal/filter"
	"sync-http/internal/lock"
	"time"

	"github.com/gorilla/websocket"
)

type Config struct {
	RootDir            string
	ListenAddr         string
	ListenPort         int
	Debug              bool
	ProcessLogPath     string
	TrustedProxies     []string
	LockDir            string
	LockTTL            time.Duration
	PageSize           int
	SnapshotTTL        time.Duration
	Modules            map[string]Module
	AuditDir           string
	AuditRetentionDays int
}

type Service struct {
	cfg       Config
	locks     *lock.Manager
	audit     *AuditWriter
	mu        sync.RWMutex
	snapshots map[string]*Snapshot
	sessions  map[string]*Session
	upgrader  websocket.Upgrader
	trusted   []netip.Prefix
	plog      *processLogger
}

type Session struct {
	ID         string
	SnapshotID string
	Module     string
	SourcePath string
	CreatedAt  time.Time
	ClientID   string
	Status     string
}

func New(cfg Config) (*Service, error) {
	if cfg.PageSize <= 0 {
		cfg.PageSize = 5000
	}
	if cfg.LockTTL <= 0 {
		cfg.LockTTL = 10 * time.Minute
	}
	if cfg.SnapshotTTL <= 0 {
		cfg.SnapshotTTL = 30 * time.Minute
	}
	if cfg.AuditRetentionDays <= 0 {
		cfg.AuditRetentionDays = 30
	}
	if cfg.LockDir == "" {
		cfg.LockDir = ".sync-locks"
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = "0.0.0.0"
	}
	if cfg.ListenPort <= 0 {
		cfg.ListenPort = 8080
	}
	if cfg.ProcessLogPath == "" {
		cfg.ProcessLogPath = "./process.log"
	}
	if cfg.AuditDir == "" {
		cfg.AuditDir = "./audit"
	}
	if len(cfg.Modules) == 0 {
		return nil, fmt.Errorf("no modules configured")
	}
	trusted, err := parseTrustedProxies(cfg.TrustedProxies)
	if err != nil {
		return nil, err
	}

	lm, err := lock.NewManager(cfg.LockDir)
	if err != nil {
		return nil, err
	}
	audit, err := NewAuditWriter(cfg.AuditDir, cfg.AuditRetentionDays)
	if err != nil {
		return nil, err
	}
	plog, err := newProcessLogger(cfg.ProcessLogPath, cfg.Debug)
	if err != nil {
		return nil, err
	}
	return &Service{
		cfg:       cfg,
		locks:     lm,
		audit:     audit,
		plog:      plog,
		snapshots: map[string]*Snapshot{},
		sessions:  map[string]*Session{},
		upgrader: websocket.Upgrader{
			CheckOrigin: func(_ *http.Request) bool { return true },
		},
		trusted: trusted,
	}, nil
}

func parseTrustedProxies(entries []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(entries))
	for _, raw := range entries {
		v := strings.TrimSpace(raw)
		if v == "" {
			continue
		}
		if strings.Contains(v, "/") {
			p, err := netip.ParsePrefix(v)
			if err != nil {
				return nil, fmt.Errorf("invalid trusted proxy %q: %w", v, err)
			}
			out = append(out, p.Masked())
			continue
		}
		ip, err := netip.ParseAddr(v)
		if err != nil {
			return nil, fmt.Errorf("invalid trusted proxy %q: %w", v, err)
		}
		bits := 32
		if ip.Is6() {
			bits = 128
		}
		out = append(out, netip.PrefixFrom(ip, bits))
	}
	return out, nil
}

func (s *Service) requestClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	remoteIP, err := netip.ParseAddr(strings.TrimSpace(host))
	if err != nil {
		return ""
	}
	if !s.isTrustedProxy(remoteIP) {
		return remoteIP.String()
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff != "" {
		parts := strings.Split(xff, ",")
		for _, p := range parts {
			ip, err := netip.ParseAddr(strings.TrimSpace(p))
			if err == nil {
				return ip.String()
			}
		}
	}
	xri := strings.TrimSpace(r.Header.Get("X-Real-IP"))
	if xri != "" {
		if ip, err := netip.ParseAddr(xri); err == nil {
			return ip.String()
		}
	}
	return remoteIP.String()
}

func (s *Service) isTrustedProxy(ip netip.Addr) bool {
	for _, p := range s.trusted {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.health)
	mux.HandleFunc("/v1/locks", s.handleLock)
	mux.HandleFunc("/v1/locks/release", s.handleUnlock)
	mux.HandleFunc("/v1/locks/wait/ws", s.handleLockWaitWS)
	mux.HandleFunc("/v1/sessions", s.handleSessionCreate)
	mux.HandleFunc("/v1/snapshots/", s.handleManifest)
	mux.HandleFunc("/v1/objects", s.handleObject)
	mux.HandleFunc("/v1/sessions/", s.handleSessionCommit)
	return s.loggingMiddleware(mux)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := s.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("response writer does not support hijacking")
	}
	return h.Hijack()
}

func (s *statusRecorder) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}

func (s *Service) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		costMs := time.Since(start).Milliseconds()
		clientIP := s.requestClientIP(r)
		s.plog.Infof("request method=%s path=%s status=%d bytes=%d cost_ms=%d client_ip=%s", r.Method, r.URL.Path, rec.status, rec.bytes, costMs, clientIP)
		s.plog.Debugf("request_details query=%q ua=%q remote=%q", r.URL.RawQuery, r.UserAgent(), r.RemoteAddr)
	})
}

func (s *Service) health(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Service) tokenFromHeader(r *http.Request) (string, error) {
	head := r.Header.Get("Authorization")
	if !strings.HasPrefix(head, "Bearer ") {
		return "", fmt.Errorf("missing token")
	}
	return strings.TrimPrefix(head, "Bearer "), nil
}

func (s *Service) authorizeModule(r *http.Request, module string) error {
	mod, ok := s.cfg.Modules[module]
	if !ok {
		return fmt.Errorf("unknown module")
	}
	tok, err := s.tokenFromHeader(r)
	if err != nil {
		return err
	}
	if _, ok := mod.Tokens[tok]; !ok {
		return fmt.Errorf("token not authorized for module")
	}
	return nil
}

func (s *Service) handleLock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	type req struct {
		Module string `json:"module"`
		Owner  string `json:"owner"`
		TTL    int    `json:"ttl_seconds"`
	}
	var body req
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if body.Module == "" || body.Owner == "" {
		http.Error(w, "module and owner required", http.StatusBadRequest)
		return
	}
	if err := s.authorizeModule(r, body.Module); err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	ttl := time.Duration(body.TTL) * time.Second
	if ttl <= 0 {
		ttl = s.cfg.LockTTL
	}
	if err := s.locks.TryAcquire("module:"+body.Module, body.Owner, ttl); err != nil {
		if errors.Is(err, lock.ErrLocked) {
			http.Error(w, err.Error(), http.StatusLocked)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.logEvent(r, "lock_acquired", body.Module, "", body.Owner)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleUnlock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	type req struct {
		Module string `json:"module"`
		Owner  string `json:"owner"`
	}
	var body req
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if body.Module == "" {
		http.Error(w, "module required", http.StatusBadRequest)
		return
	}
	if err := s.authorizeModule(r, body.Module); err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	s.locks.Release("module:"+body.Module, body.Owner)
	s.logEvent(r, "lock_released", body.Module, "", body.Owner)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) handleLockWaitWS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	module := r.URL.Query().Get("module")
	if module == "" {
		http.Error(w, "module required", http.StatusBadRequest)
		return
	}
	if err := s.authorizeModule(r, module); err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	s.logEvent(r, "ws_wait_start", module, "", "")
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		locked, owner := s.locks.IsLocked("module:" + module)
		if !locked {
			_ = conn.WriteJSON(map[string]string{"event": "unlocked", "module": module})
			s.logEvent(r, "ws_wait_unlocked", module, "", "")
			return
		}
		if err := conn.WriteJSON(map[string]string{"event": "locked", "module": module, "owner": owner}); err != nil {
			s.logEvent(r, "ws_wait_disconnect", module, "", "")
			return
		}
		select {
		case <-r.Context().Done():
			s.logEvent(r, "ws_wait_context_done", module, "", "")
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) handleSessionCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req common.SessionCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Module) == "" {
		http.Error(w, "module is required", http.StatusBadRequest)
		return
	}
	if err := s.authorizeModule(r, req.Module); err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	mod := s.cfg.Modules[req.Module]

	if locked, owner := s.locks.IsLocked("module:" + req.Module); locked {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusLocked)
		_ = json.NewEncoder(w).Encode(common.SessionCreateResponse{Status: "locked", Message: "locked by " + owner, RetryAfter: 1})
		s.logEvent(r, "session_locked", req.Module, "", owner)
		return
	}

	sourcePath := filepath.Clean(req.SourcePath)
	if sourcePath == "." || sourcePath == "" {
		sourcePath = "."
	}
	src := filepath.Join(mod.RootDir, sourcePath)
	absSrc, err := filepath.Abs(src)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	relToModule, err := filepath.Rel(mod.RootDir, absSrc)
	if err != nil || strings.HasPrefix(relToModule, "..") {
		http.Error(w, "source path escapes module root", http.StatusBadRequest)
		return
	}
	if _, err := os.Stat(absSrc); err != nil {
		http.Error(w, fmt.Sprintf("invalid source path: %v", err), http.StatusBadRequest)
		return
	}

	ex, err := filter.NewExcluder(req.ExcludeGlobs, req.ExcludeRegex)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sid := strconv.FormatInt(time.Now().UnixNano(), 36)
	snapshotID := "snap_" + sid
	snap, err := BuildSnapshot(snapshotID, absSrc, ex)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sess := &Session{ID: sid, SnapshotID: snapshotID, Module: req.Module, SourcePath: sourcePath, CreatedAt: time.Now(), ClientID: req.ClientID, Status: "active"}

	s.mu.Lock()
	s.snapshots[snapshotID] = snap
	s.sessions[sid] = sess
	s.cleanupExpiredLocked()
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(common.SessionCreateResponse{
		SessionID:   sid,
		SnapshotID:  snapshotID,
		Status:      "ready",
		ManifestURL: "/v1/snapshots/" + snapshotID + "/manifest",
	})
	s.logEvent(r, "session_created", req.Module, sid, "")
}

func (s *Service) handleManifest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !strings.HasSuffix(r.URL.Path, "/manifest") {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 5 {
		http.NotFound(w, r)
		return
	}
	snapshotID := parts[3]

	module, err := s.moduleForSnapshot(snapshotID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if err := s.authorizeModule(r, module); err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	s.mu.RLock()
	snap := s.snapshots[snapshotID]
	s.mu.RUnlock()
	if snap == nil {
		http.NotFound(w, r)
		return
	}

	pageSize := s.cfg.PageSize
	if q := r.URL.Query().Get("page_size"); q != "" {
		if i, err := strconv.Atoi(q); err == nil && i > 0 && i <= 20000 {
			pageSize = i
		}
	}
	cursor := 0
	if q := r.URL.Query().Get("cursor"); q != "" {
		i, err := strconv.Atoi(q)
		if err == nil && i >= 0 {
			cursor = i
		}
	}
	if cursor > len(snap.Entries) {
		cursor = len(snap.Entries)
	}
	end := cursor + pageSize
	if end > len(snap.Entries) {
		end = len(snap.Entries)
	}
	next := ""
	if end < len(snap.Entries) {
		next = strconv.Itoa(end)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(common.ManifestPageResponse{SnapshotID: snapshotID, Entries: snap.Entries[cursor:end], NextCursor: next})
}

func (s *Service) handleObject(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	snapshotID := r.URL.Query().Get("snapshot_id")
	p := r.URL.Query().Get("path")
	if snapshotID == "" || p == "" {
		http.Error(w, "snapshot_id and path required", http.StatusBadRequest)
		return
	}

	module, err := s.moduleForSnapshot(snapshotID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	if err := s.authorizeModule(r, module); err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	s.mu.RLock()
	snap := s.snapshots[snapshotID]
	s.mu.RUnlock()
	if snap == nil {
		http.NotFound(w, r)
		return
	}
	entry, ok := snap.ByPath[p]
	if !ok {
		http.NotFound(w, r)
		return
	}
	abs := filepath.Join(snap.SourceDir, filepath.FromSlash(p))
	f, err := os.Open(abs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()

	w.Header().Set("ETag", entry.Checksum)
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, entry.Path, time.Unix(entry.Mtime, 0), f)
}

func (s *Service) handleSessionCommit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/commit") {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 5 {
		http.NotFound(w, r)
		return
	}
	sid := parts[3]

	s.mu.RLock()
	sess := s.sessions[sid]
	s.mu.RUnlock()
	if sess == nil {
		http.NotFound(w, r)
		return
	}
	if err := s.authorizeModule(r, sess.Module); err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = body

	s.mu.Lock()
	if sess := s.sessions[sid]; sess != nil {
		sess.Status = "committed"
	}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(common.SessionCommitResponse{Status: "ok"})
	s.logEvent(r, "session_committed", sess.Module, sid, "")
}

func (s *Service) cleanupExpiredLocked() {
	now := time.Now()
	for id, sess := range s.sessions {
		if now.Sub(sess.CreatedAt) > s.cfg.SnapshotTTL {
			delete(s.sessions, id)
			delete(s.snapshots, sess.SnapshotID)
		}
	}
}

func (s *Service) moduleForSnapshot(snapshotID string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sess := range s.sessions {
		if sess.SnapshotID == snapshotID {
			return sess.Module, nil
		}
	}
	return "", fmt.Errorf("unknown snapshot")
}

func (s *Service) logEvent(r *http.Request, event, module, sessionID, owner string) {
	clientIP := ""
	if r != nil {
		clientIP = s.requestClientIP(r)
	}
	line := fmt.Sprintf("event=%s module=%s session=%s owner=%s", event, module, sessionID, owner)
	s.plog.Infof("%s", line)
	s.plog.Debugf("event_details client_ip=%s", clientIP)
	s.audit.Write(map[string]any{
		"event":      event,
		"module":     module,
		"session_id": sessionID,
		"owner":      owner,
		"client_ip":  clientIP,
	})
}
