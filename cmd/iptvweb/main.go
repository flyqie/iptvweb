package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"iptvweb/internal/config"
	"iptvweb/internal/m3u"
	"iptvweb/internal/scheduler"
	"iptvweb/internal/server"
)

func main() {
	cfg, err := config.Load("config.toml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		fmt.Fprintln(os.Stderr, "please copy config.toml.example to config.toml and configure it")
		os.Exit(1)
	}

	if cfg.M3U.URL == "" {
		fmt.Fprintln(os.Stderr, "error: m3u.url is not set in config.toml")
		os.Exit(1)
	}

	store := m3u.NewStore()
	sched := scheduler.New(store, cfg.M3U.URL, cfg.M3U.UpdateInterval)

	fmt.Println("[main] fetching initial M3U playlist...")
	if err := m3u.FetchAndStore(store, cfg.M3U.URL); err != nil {
		fmt.Fprintf(os.Stderr, "warning: initial fetch failed: %v\n", err)
		fmt.Fprintln(os.Stderr, "you can retry via the web UI")
	}

	if cfg.M3U.AutoUpdate > 0 {
		if err := sched.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to start scheduler: %v\n", err)
			os.Exit(1)
		}
		defer sched.Stop()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := server.New(cfg.ServerAddr, cfg.SecretKey, store, sched, cfg.M3U.DetectMode)
	if err := srv.StartContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "server error: %v\n", err)
		os.Exit(1)
	}
}
