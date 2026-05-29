package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/maxdubrinsky/plex-mirror/internal/config"
	"github.com/maxdubrinsky/plex-mirror/internal/db"
	"github.com/maxdubrinsky/plex-mirror/internal/server"
	"github.com/maxdubrinsky/plex-mirror/internal/service"
)

func main() {
	// Subcommands are positional and consume the rest of os.Args themselves.
	// Anything starting with `-` falls through to the server-mode flag parser
	// below, so the existing `-healthcheck` docker HEALTHCHECK contract is
	// preserved.
	if len(os.Args) > 1 && len(os.Args[1]) > 0 && os.Args[1][0] != '-' {
		switch os.Args[1] {
		case "dump":
			os.Exit(cmdDump(os.Args[2:]))
		case "evict-now":
			os.Exit(cmdEvictNow(os.Args[2:]))
		case "download":
			os.Exit(cmdDownload(os.Args[2:]))
		default:
			fmt.Fprintf(os.Stderr, "plex-mirror: unknown subcommand %q\n", os.Args[1])
			fmt.Fprintln(os.Stderr, "usage: plex-mirror [server] | dump --source=plex|jellyfin | download --source=plex --item=ID | evict-now")
			os.Exit(2)
		}
	}

	healthcheck := flag.Bool("healthcheck", false, "probe local /healthz over loopback and exit 0/1 (for docker HEALTHCHECK)")
	flag.Parse()

	if *healthcheck {
		os.Exit(runHealthcheck())
	}

	cfg, err := config.Load()
	if err != nil {
		slog.Error("config load failed", "err", err)
		os.Exit(1)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.SlogLevel(),
	}))
	slog.SetDefault(logger)

	store, err := db.Open(cfg.DBPath)
	if err != nil {
		slog.Error("db open failed", "path", cfg.DBPath, "err", err)
		os.Exit(1)
	}
	defer store.Close()

	if err := store.Migrate(context.Background()); err != nil {
		slog.Error("db migrate failed", "err", err)
		os.Exit(1)
	}

	svc, err := service.New(cfg, store)
	if err != nil {
		slog.Error("service init failed", "err", err)
		os.Exit(1)
	}

	srv := server.New(cfg, store, svc)
	httpServer := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Start background workers: the storage sweeper plus, when Plex is
	// configured, the download daemon. Both return immediately if their
	// interval is zero and stop cleanly on ctx cancellation. Browse + MCP work
	// regardless of whether the engine is running.
	svc.Start(ctx)

	go func() {
		slog.Info("listening", "addr", cfg.HTTPAddr, "media_root", cfg.MediaRoot, "db_path", cfg.DBPath)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("http server error", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown error", "err", err)
		os.Exit(1)
	}
}

// runHealthcheck dials this container's own /healthz. Distroless has no
// shell or curl, so the binary doubles as its own probe via `-healthcheck`.
func runHealthcheck() int {
	addr := os.Getenv("PLEXMIRROR_HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	// Convert :PORT or 0.0.0.0:PORT to localhost:PORT for loopback probe.
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: bad PLEXMIRROR_HTTP_ADDR %q: %v\n", addr, err)
		return 1
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	url := "http://" + net.JoinHostPort(host, port) + "/healthz"

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck: status %s\n", resp.Status)
		return 1
	}
	return 0
}
