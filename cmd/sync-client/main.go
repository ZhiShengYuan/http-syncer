package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"strings"
	"sync-http/internal/client"
	"time"
)

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func main() {
	var (
		serverURL           = flag.String("server", "http://127.0.0.1:8080", "sync server URL")
		token               = flag.String("token", "", "bearer token")
		userAgent           = flag.String("user-agent", "go-sync/1.0", "HTTP User-Agent sent to server")
		module              = flag.String("module", "", "module name on server (required)")
		sourcePath          = flag.String("source", ".", "source path on server")
		targetDir           = flag.String("target", "./mirror", "local mirror target directory")
		clientID            = flag.String("client-id", "sync-client", "client id")
		debug               = flag.Bool("debug", false, "enable debug-level client logs")
		processLog          = flag.String("process-log", "", "client process log file path (default stdout)")
		deleteGuardRatio    = flag.Float64("delete-guard-ratio", 0.10, "abort when delete ratio exceeds this")
		deleteGuardMinFiles = flag.Int("delete-guard-min-files", 1000, "enable delete guard only when local files >= this")
		forceDeleteGuard    = flag.Bool("force-delete-guard", false, "override delete guard")
		dryRun              = flag.Bool("dry-run", false, "plan only, no writes")
		pageSize            = flag.Int("page-size", 5000, "manifest page size")
		downloadConcurrency = flag.Int("download-concurrency", 8, "number of concurrent file downloads")
	)
	var exGlobs, exRegex stringList
	flag.Var(&exGlobs, "exclude-glob", "exclude glob pattern (repeatable)")
	flag.Var(&exRegex, "exclude-regex", "exclude regex pattern (repeatable)")
	flag.Parse()

	ctx := context.Background()
	res, err := client.Run(ctx, client.Config{
		ServerURL:           *serverURL,
		Token:               *token,
		UserAgent:           *userAgent,
		Module:              *module,
		SourcePath:          *sourcePath,
		TargetDir:           *targetDir,
		ClientID:            *clientID,
		Debug:               *debug,
		ProcessLogPath:      *processLog,
		ExcludeGlobs:        exGlobs,
		ExcludeRegex:        exRegex,
		DeleteGuardRatio:    *deleteGuardRatio,
		DeleteGuardMinFiles: *deleteGuardMinFiles,
		ForceDeleteGuard:    *forceDeleteGuard,
		DryRun:              *dryRun,
		PageSize:            *pageSize,
		DownloadConcurrency: *downloadConcurrency,
		Backoffs:            []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second},
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("session=%s snapshot=%s downloaded=%d deleted=%d bytes=%d\n", res.SessionID, res.SnapshotID, res.Downloaded, res.Deleted, res.Bytes)
}
