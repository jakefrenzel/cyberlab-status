package status

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestEvaluate(t *testing.T) {
	c := Check{Warn: 1, Crit: 5}
	for _, tc := range []struct {
		value float64
		want  Level
	}{
		{0, OK}, {0.5, OK}, {1, Warn}, {4, Warn}, {5, Crit}, {100, Crit},
	} {
		if got := c.Evaluate(tc.value); got != tc.want {
			t.Errorf("Evaluate(%v) = %v, want %v", tc.value, got, tc.want)
		}
	}

	// A zero threshold is disabled, not "always triggers"
	critOnly := Check{Crit: 1}
	if got := critOnly.Evaluate(0); got != OK {
		t.Errorf("crit-only Evaluate(0) = %v, want ok", got)
	}
	if got := critOnly.Evaluate(1); got != Crit {
		t.Errorf("crit-only Evaluate(1) = %v, want crit", got)
	}
}

func TestPrometheusQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			http.NotFound(w, r)
			return
		}
		switch r.URL.Query().Get("query") {
		case "one":
			w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"3"]}]}}`))
		case "two":
			w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[` +
				`{"metric":{"alertname":"A"},"value":[1700000000,"1"]},` +
				`{"metric":{"alertname":"B"},"value":[1700000000,"1"]}]}}`))
		case "scalar":
			w.Write([]byte(`{"status":"success","data":{"resultType":"scalar","result":[1700000000,"0.25"]}}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"status":"error","errorType":"bad_data","error":"parse error"}`))
		}
	}))
	defer srv.Close()

	p, err := NewPrometheus(srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if v, err := p.QueryValue(ctx, "one"); err != nil || v != 3 {
		t.Errorf("QueryValue(one) = %v, %v; want 3, nil", v, err)
	}
	if v, err := p.QueryValue(ctx, "scalar"); err != nil || v != 0.25 {
		t.Errorf("QueryValue(scalar) = %v, %v; want 0.25, nil", v, err)
	}
	if _, err := p.QueryValue(ctx, "two"); err == nil {
		t.Error("QueryValue(two) succeeded; want an error for 2 series")
	}
	if _, err := p.QueryValue(ctx, "bad"); err == nil {
		t.Error("QueryValue(bad) succeeded; want the API error")
	}
	samples, err := p.Query(ctx, "two")
	if err != nil || len(samples) != 2 || samples[1].Labels["alertname"] != "B" {
		t.Errorf("Query(two) = %+v, %v", samples, err)
	}
}

func TestNewPrometheusRejectsBadURLs(t *testing.T) {
	for _, u := range []string{"", "prometheus:9090", "ftp://prometheus", "http://"} {
		if _, err := NewPrometheus(u, http.DefaultClient); err == nil {
			t.Errorf("NewPrometheus(%q) succeeded; want an error", u)
		}
	}
}

// fakeProm answers each query from a map; a missing query is an error.
type fakeProm struct {
	values map[string]float64
	alerts []Sample
	calls  atomic.Int32
}

func (f *fakeProm) QueryValue(_ context.Context, expr string) (float64, error) {
	f.calls.Add(1)
	v, ok := f.values[expr]
	if !ok {
		return 0, errors.New("no such query")
	}
	return v, nil
}

func (f *fakeProm) Query(_ context.Context, _ string) ([]Sample, error) {
	return f.alerts, nil
}

var testChecks = []Check{
	{ID: "a", Section: "Cluster", Name: "A", Query: "qa", Warn: 1, Crit: 5},
	{ID: "b", Section: "Network", Name: "B", Query: "qb", Crit: 1},
}

func TestCheckerOverall(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values map[string]float64
		alerts []Sample
		want   Level
	}{
		{"all zero", map[string]float64{"qa": 0, "qb": 0}, nil, OK},
		{"one warning", map[string]float64{"qa": 2, "qb": 0}, nil, Warn},
		{"one critical", map[string]float64{"qa": 2, "qb": 1}, nil, Crit},
		{"query fails", map[string]float64{"qa": 0}, nil, Unknown},
		{"warning alert", map[string]float64{"qa": 0, "qb": 0},
			[]Sample{{Labels: map[string]string{"alertname": "X", "severity": "warning"}}}, Warn},
		{"critical alert", map[string]float64{"qa": 0, "qb": 0},
			[]Sample{{Labels: map[string]string{"alertname": "X", "severity": "critical"}}}, Crit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewChecker(&fakeProm{values: tc.values, alerts: tc.alerts}, testChecks, time.Minute, prometheus.NewRegistry())
			if got := c.Snapshot(context.Background()).Overall; got != tc.want {
				t.Errorf("Overall = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCheckerCaches(t *testing.T) {
	now := time.Unix(1700000000, 0)
	fake := &fakeProm{values: map[string]float64{"qa": 0, "qb": 0}}
	c := NewChecker(fake, testChecks, 15*time.Second, prometheus.NewRegistry())
	c.now = func() time.Time { return now }

	ctx := context.Background()
	c.Snapshot(ctx)
	c.Snapshot(ctx)
	if got := fake.calls.Load(); got != 2 {
		t.Fatalf("after two snapshots within the TTL: %d queries, want 2 (one refresh)", got)
	}

	now = now.Add(16 * time.Second)
	c.Snapshot(ctx)
	if got := fake.calls.Load(); got != 4 {
		t.Fatalf("after the TTL expired: %d queries, want 4 (two refreshes)", got)
	}
}

func TestAlertsSortedCriticalFirst(t *testing.T) {
	fake := &fakeProm{
		values: map[string]float64{"qa": 0, "qb": 0},
		alerts: []Sample{
			{Labels: map[string]string{"alertname": "Zeta", "severity": "warning"}},
			{Labels: map[string]string{"alertname": "Beta", "severity": "critical"}},
			{Labels: map[string]string{"alertname": "Alpha", "severity": "warning"}},
		},
	}
	c := NewChecker(fake, testChecks, time.Minute, prometheus.NewRegistry())
	alerts := c.Snapshot(context.Background()).Alerts
	got := []string{alerts[0].Name, alerts[1].Name, alerts[2].Name}
	want := []string{"Beta", "Alpha", "Zeta"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("alert order = %v, want %v", got, want)
		}
	}
}

func TestDefaultChecksAreValid(t *testing.T) {
	seen := map[string]bool{}
	sections := map[string]bool{}
	for _, s := range Sections {
		sections[s] = true
	}
	for _, c := range DefaultChecks {
		if c.ID == "" || c.Name == "" || c.Query == "" {
			t.Errorf("check %+v is missing an ID, name, or query", c)
		}
		if seen[c.ID] {
			t.Errorf("duplicate check ID %q", c.ID)
		}
		seen[c.ID] = true
		if !sections[c.Section] {
			t.Errorf("check %q is in unknown section %q", c.ID, c.Section)
		}
		if c.Warn == 0 && c.Crit == 0 {
			t.Errorf("check %q has no thresholds, so it can never turn amber or red", c.ID)
		}
	}
}
