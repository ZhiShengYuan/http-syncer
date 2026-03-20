package main

import (
	"flag"
	"log"
	"net/http"
	"sync-http/internal/server"
)

func main() {
	var (
		listen     = flag.String("listen", ":8080", "listen address")
		configPath = flag.String("config", "./server.yaml", "server YAML config")
	)
	flag.Parse()

	cfg, err := server.LoadFileConfig(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	svc, err := server.New(*cfg)
	if err != nil {
		log.Fatalf("init server: %v", err)
	}

	log.Printf("sync-server starting on %s config=%s", *listen, *configPath)
	if err := http.ListenAndServe(*listen, svc.Handler()); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}
