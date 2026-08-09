package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/hightemp/https_proxy/internal/config"
	"github.com/hightemp/https_proxy/internal/proxy"
)

func main() {
	configPath := flag.String("config", "config.yaml", "Path to the config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("Error loading config file", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := proxy.Run(ctx, cfg); err != nil {
		slog.Error("Proxy server stopped with error", "error", err)
		os.Exit(1)
	}
	slog.Info("Server stopped")
}
