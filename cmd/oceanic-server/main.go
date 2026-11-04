// Command oceanic-server runs the OCeanic RMM control plane.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/imshodj/oceanic/internal/server"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(server.HealthCheck())
	}
	cfg := server.ConfigFromEnv()
	s, err := server.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := s.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
