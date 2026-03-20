package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"sync-http/internal/server"
)

func main() {
	var (
		listen       = flag.String("listen", "", "listen address, e.g. 0.0.0.0:8080 (overrides addr/port)")
		listenAddr   = flag.String("listen-addr", "", "listen IP/host override, e.g. 0.0.0.0")
		listenPort   = flag.Int("listen-port", 0, "listen port override")
		trustedProxy stringList
		configPath   = flag.String("config", "./server.yaml", "server YAML config")
	)
	flag.Var(&trustedProxy, "trusted-proxy", "trusted proxy IP/CIDR override (repeatable)")
	flag.Parse()

	cfg, err := server.LoadFileConfig(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	if *listenAddr != "" {
		cfg.ListenAddr = *listenAddr
	}
	if *listenPort > 0 {
		cfg.ListenPort = *listenPort
	}
	if len(trustedProxy) > 0 {
		cfg.TrustedProxies = trustedProxy
	}

	svc, err := server.New(*cfg)
	if err != nil {
		log.Fatalf("init server: %v", err)
	}

	bind := *listen
	if bind == "" {
		bind = fmt.Sprintf("%s:%d", cfg.ListenAddr, cfg.ListenPort)
	}

	log.Printf("sync-server starting on %s config=%s", bind, *configPath)
	if err := http.ListenAndServe(bind, svc.Handler()); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}

type stringList []string

func (s *stringList) String() string { return fmt.Sprintf("%v", []string(*s)) }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}
