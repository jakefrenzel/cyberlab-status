// Command cyberlab-status serves a one-page health summary of the cyberlab cluster,
// built from Prometheus queries.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/jakefrenzel/cyberlab-status/internal/status"
	"github.com/jakefrenzel/cyberlab-status/internal/web"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func run() error {
	listen := flag.String("listen", envOr("LISTEN_ADDR", ":8080"), "address to serve on (env LISTEN_ADDR)")
	promURL := flag.String("prometheus", envOr("PROMETHEUS_URL",
		"http://kube-prometheus-stack-prometheus.monitoring.svc:9090"),
		"Prometheus base URL (env PROMETHEUS_URL)")
	cacheTTL := flag.Duration("cache", 15*time.Second, "how long to reuse query results")
	flag.Parse()

	prom, err := status.NewPrometheus(*promURL, &http.Client{Timeout: 5 * time.Second})
	if err != nil {
		return err
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	checker := status.NewChecker(prom, status.DefaultChecks, *cacheTTL, reg)

	srv := &http.Server{
		Addr:    *listen,
		Handler: web.NewHandler(checker, reg),
		// Timeouts so a slow or idle client can't hold connections open forever
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	// Kubernetes sends SIGTERM before killing a pod: finish in-flight requests first
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", *listen, "prometheus", *promURL)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	return nil
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}
