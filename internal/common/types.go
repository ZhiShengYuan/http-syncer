package common

type SessionCreateRequest struct {
	Module        string   `json:"module"`
	SourcePath    string   `json:"source_path"`
	ExcludeGlobs  []string `json:"exclude_globs"`
	ExcludeRegex  []string `json:"exclude_regex"`
	ClientID      string   `json:"client_id"`
	RequestedPage int      `json:"requested_page_size"`
}

type SessionCreateResponse struct {
	SessionID   string `json:"session_id,omitempty"`
	SnapshotID  string `json:"snapshot_id,omitempty"`
	Status      string `json:"status"`
	Message     string `json:"message,omitempty"`
	RetryAfter  int    `json:"retry_after_seconds,omitempty"`
	ManifestURL string `json:"manifest_url,omitempty"`
}

type ManifestPageResponse struct {
	SnapshotID string          `json:"snapshot_id"`
	Entries    []ManifestEntry `json:"entries"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

type ManifestEntry struct {
	Path     string `json:"path"`
	Type     string `json:"type"`
	Size     int64  `json:"size"`
	Mode     uint32 `json:"mode"`
	Mtime    int64  `json:"mtime"`
	Checksum string `json:"checksum"`
}

type SessionCommitRequest struct {
	Downloaded int    `json:"downloaded"`
	Deleted    int    `json:"deleted"`
	Bytes      int64  `json:"bytes"`
	Status     string `json:"status"`
}

type SessionCommitResponse struct {
	Status string `json:"status"`
}
