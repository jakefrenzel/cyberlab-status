// Package web serves the status page, its JSON API, and the health and metrics
// endpoints Kubernetes and Prometheus use.
package web

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"html/template"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/jakefrenzel/cyberlab-status/internal/status"
)

//go:embed templates static
var assets embed.FS

// Snapshotter is what the handlers need from the checker; tests swap in a fake.
type Snapshotter interface {
	Snapshot(ctx context.Context) status.Snapshot
}

type handler struct {
	checker Snapshotter
	page    *template.Template
}

// NewHandler returns the app's routes. reg is both where the HTTP metrics are
// registered and what /metrics serves.
func NewHandler(checker Snapshotter, reg *prometheus.Registry) http.Handler {
	page := template.Must(template.New("index.html").Funcs(template.FuncMap{
		"section": sectionResults,
		"value":   formatValue,
		"ago":     func(t time.Time) string { return time.Since(t).Round(time.Second).String() },
	}).ParseFS(assets, "templates/index.html"))

	h := &handler{checker: checker, page: page}
	static, _ := fs.Sub(assets, "static")

	requests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cyberlab_status_http_requests_total",
		Help: "HTTP requests served, by route and status code.",
	}, []string{"route", "code"})
	reg.MustRegister(requests)
	count := func(route string, next http.Handler) http.Handler {
		return promhttp.InstrumentHandlerCounter(requests.MustCurryWith(prometheus.Labels{"route": route}), next)
	}

	mux := http.NewServeMux()
	// {$} matches "/" exactly; anything else unknown is a 404, not the page
	mux.Handle("GET /{$}", count("page", http.HandlerFunc(h.servePage)))
	mux.Handle("GET /api/status", count("api", http.HandlerFunc(h.serveJSON)))
	mux.Handle("GET /static/", count("static", http.StripPrefix("/static/", http.FileServerFS(static))))
	mux.HandleFunc("GET /healthz", serveHealthz)
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	return securityHeaders(mux)
}

func (h *handler) servePage(w http.ResponseWriter, r *http.Request) {
	snap := h.checker.Snapshot(r.Context())
	// Render into a buffer first, so a template error becomes a clean 500
	// instead of half a page
	var buf bytes.Buffer
	if err := h.page.Execute(&buf, pageData{Snapshot: snap, Sections: status.Sections}); err != nil {
		slog.Error("render page", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = buf.WriteTo(w)
}

func (h *handler) serveJSON(w http.ResponseWriter, r *http.Request) {
	snap := h.checker.Snapshot(r.Context())
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(snap); err != nil {
		slog.Error("encode status", "err", err)
	}
}

// Liveness only: the process is up and serving. Prometheus being down makes the
// page show "unknown", but restarting this pod wouldn't fix that.
func serveHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

// securityHeaders locks the page down: no scripts at all, styles only from this
// origin, and no framing, so injected markup can't do anything.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'self'; img-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

type pageData struct {
	status.Snapshot
	Sections []string
}

func sectionResults(results []status.Result, section string) []status.Result {
	var out []status.Result
	for _, r := range results {
		if r.Section == section {
			out = append(out, r)
		}
	}
	return out
}

// Counts are whole numbers; show them without a trailing ".0"
func formatValue(r status.Result) string {
	if r.Level == status.Unknown || math.IsNaN(r.Value) {
		return "?"
	}
	return strconv.FormatFloat(r.Value, 'f', -1, 64)
}
