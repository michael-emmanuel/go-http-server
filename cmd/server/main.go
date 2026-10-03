// Command server runs the HTTP service.
//
// main only translates the process environment (env vars, signals, exit code)
// into calls on the internal packages. Everything else lives where it can be
// tested.
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/example/go-production-http-server/internal/config"
	"github.com/example/go-production-http-server/internal/logging"
	"github.com/example/go-production-http-server/internal/server"
)

// version is overridden at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

func main() {
	// os.Exit does not run deferred functions, so it is confined to this
	// one line, after run has returned and all of its defers have executed.
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}

	// "server healthcheck" probes the local instance. It exists because the
	// container image has no shell, curl, or wget to run a HEALTHCHECK with.
	if len(args) > 0 && args[0] == "healthcheck" {
		return healthcheck(cfg.Addr)
	}
	if len(args) > 0 {
		return fmt.Errorf("unknown command %q (the only subcommand is \"healthcheck\")", args[0])
	}

	log := logging.New(os.Stdout, cfg.LogLevel)

	srv, err := server.New(cfg, log, server.WithVersion(version))
	if err != nil {
		return fmt.Errorf("build server: %w", err)
	}

	// SIGINT is Ctrl-C; SIGTERM is what Kubernetes and Docker send on stop.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		// After the first signal, restore default signal behavior so a second
		// Ctrl-C terminates immediately if graceful shutdown hangs.
		<-ctx.Done()
		stop()
	}()

	return srv.ListenAndServe(ctx)
}

// healthcheck performs GET /health/live against this instance's address.
func healthcheck(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("parse HTTP_ADDR: %w", err)
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	url := "http://" + net.JoinHostPort(host, port) + "/health/live"

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("probe %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("probe %s: status %d", url, resp.StatusCode)
	}
	return nil
}
