package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/jakefrenzel/cyberlab-status/internal/status"
)

type fakeChecker struct{ snap status.Snapshot }

func (f fakeChecker) Snapshot(context.Context) status.Snapshot { return f.snap }

func newTestHandler(snap status.Snapshot) http.Handler {
	return NewHandler(fakeChecker{snap}, prometheus.NewRegistry(), "1.2.3")
}

var sample = status.Snapshot{
	Overall: status.Warn,
	Results: []status.Result{
		{Check: status.Check{ID: "nodes", Section: "Cluster", Name: "Nodes not ready"}, Value: 0, Level: status.OK},
		{Check: status.Check{ID: "drops", Section: "Network", Name: "Policy drops (1h)"}, Value: 211, Level: status.Warn},
		{Check: status.Check{ID: "falco", Section: "Security", Name: "Falco events (24h)"}, Level: status.Unknown, Error: "couldn't query Prometheus"},
	},
	Alerts:    []status.Alert{{Name: "CiliumPolicyDropsHigh", Severity: "warning"}},
	CheckedAt: time.Now(),
}

func get(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, path, nil))
	return rec
}

func TestPage(t *testing.T) {
	rec := get(t, newTestHandler(sample), http.MethodGet, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Something needs a look", // overall warn
		"Policy drops (1h)", ">211<",
		"CiliumPolicyDropsHigh",
		">?<", // unknown check
		"couldn&#39;t query Prometheus",
		"cyberlab-status 1.2.3", // version in the footer
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page is missing %q", want)
		}
	}
}

func TestPageEscapesLabels(t *testing.T) {
	snap := sample
	snap.Alerts = []status.Alert{{Name: `<script>alert(1)</script>`, Severity: "warning"}}
	body := get(t, newTestHandler(snap), http.MethodGet, "/").Body.String()
	if strings.Contains(body, "<script>") {
		t.Error("alert name was rendered unescaped")
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := newTestHandler(sample)
	for _, path := range []string{"/", "/healthz", "/api/status", "/static/style.css", "/nope"} {
		rec := get(t, h, http.MethodGet, path)
		csp := rec.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "script-src") {
			t.Errorf("%s: Content-Security-Policy = %q", path, csp)
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q", path, got)
		}
	}
}

func TestRoutes(t *testing.T) {
	h := newTestHandler(sample)
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/", http.StatusOK},
		{http.MethodGet, "/healthz", http.StatusOK},
		{http.MethodGet, "/metrics", http.StatusOK},
		{http.MethodGet, "/static/style.css", http.StatusOK},
		{http.MethodGet, "/index.html", http.StatusNotFound},
		{http.MethodGet, "/main.go", http.StatusNotFound},
		{http.MethodPost, "/", http.StatusMethodNotAllowed},
		{http.MethodDelete, "/api/status", http.StatusMethodNotAllowed},
	} {
		if got := get(t, h, tc.method, tc.path).Code; got != tc.want {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, got, tc.want)
		}
	}

	// Path traversal: the mux cleans "/static/../main.go" and redirects to
	// "/main.go" (a 404 above) instead of serving anything outside static/
	rec := get(t, h, http.MethodGet, "/static/../main.go")
	if rec.Code != http.StatusTemporaryRedirect || rec.Header().Get("Location") != "/main.go" {
		t.Errorf("GET /static/../main.go = %d to %q, want 307 to /main.go", rec.Code, rec.Header().Get("Location"))
	}
}

func TestJSON(t *testing.T) {
	rec := get(t, newTestHandler(sample), http.MethodGet, "/api/status")
	var got struct {
		Version string `json:"version"`
		Overall string `json:"overall"`
		Checks  []struct {
			ID    string `json:"id"`
			Level string `json:"level"`
			Query string `json:"query"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body)
	}
	if got.Version != "1.2.3" || got.Overall != "warn" || len(got.Checks) != 3 || got.Checks[1].Level != "warn" {
		t.Errorf("unexpected JSON: %+v", got)
	}
	// PromQL stays server-side
	if strings.Contains(rec.Body.String(), `"query"`) {
		t.Error("JSON exposes the checks' queries")
	}
}

func TestMetricsCountsRequests(t *testing.T) {
	reg := prometheus.NewRegistry()
	h := NewHandler(fakeChecker{sample}, reg, "1.2.3")
	get(t, h, http.MethodGet, "/")
	body := get(t, h, http.MethodGet, "/metrics").Body.String()
	if !strings.Contains(body, `cyberlab_status_http_requests_total{code="200",route="page"} 1`) {
		t.Errorf("request counter missing from /metrics:\n%s", body)
	}
}
