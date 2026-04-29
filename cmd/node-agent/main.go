package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"neutrino/internal/agent"
)

func main() {
	cfg := agent.ConfigFromEnv()

	ag, err := agent.New(cfg)
	if err != nil {
		log.Fatalf("failed to create agent: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		log.Println("shutting down...")
		cancel()
	}()

	if err := ag.Run(ctx); err != nil {
		log.Fatalf("agent error: %v", err)
	}
}
