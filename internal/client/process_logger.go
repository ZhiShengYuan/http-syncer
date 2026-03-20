package client

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
)

type processLogger struct {
	debug bool
	mu    sync.Mutex
	base  *log.Logger
}

func newProcessLogger(path string, debug bool) (*processLogger, error) {
	var w io.Writer
	if path == "" || path == "-" {
		w = os.Stdout
	} else {
		dir := filepath.Dir(path)
		if dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, err
			}
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, err
		}
		w = f
	}
	return &processLogger{
		debug: debug,
		base:  log.New(w, "", log.LstdFlags|log.Lmicroseconds|log.LUTC),
	}, nil
}

func (p *processLogger) Infof(format string, args ...any) {
	p.logf("INFO", format, args...)
}

func (p *processLogger) Debugf(format string, args ...any) {
	if p == nil || !p.debug {
		return
	}
	p.logf("DEBUG", format, args...)
}

func (p *processLogger) logf(level, format string, args ...any) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.base.Print(level + " " + fmt.Sprintf(format, args...))
}
