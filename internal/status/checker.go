package status

import (
	"cmp"
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Result is a check after it ran.
type Result struct {
	Check
	Value float64 `json:"value"`
	Level Level   `json:"level"`
	Error string  `json:"error,omitempty"`
}

// Alert is a firing Prometheus alert.
type Alert struct {
	Name      string `json:"name"`
	Severity  string `json:"severity"`
	Namespace string `json:"namespace,omitempty"`
}

// Snapshot is the whole page's data at one point in time.
type Snapshot struct {
	Overall   Level     `json:"overall"`
	Results   []Result  `json:"checks"`
	Alerts    []Alert   `json:"alerts"`
	CheckedAt time.Time `json:"checkedAt"`
}

// Querier is the part of the Prometheus client the checker needs; tests swap in a fake.
type Querier interface {
	Query(ctx context.Context, expr string) ([]Sample, error)
	QueryValue(ctx context.Context, expr string) (float64, error)
}

// Checker runs the checks and caches the snapshot for ttl, so a page refresh from
// every open browser tab doesn't turn into a burst of queries against Prometheus.
type Checker struct {
	prom    Querier
	checks  []Check
	ttl     time.Duration
	timeout time.Duration
	now     func() time.Time

	mu     sync.Mutex
	cached *Snapshot

	queryErrors     *prometheus.CounterVec
	refreshDuration prometheus.Histogram
	checkLevel      *prometheus.GaugeVec
}

// NewChecker registers the checker's own metrics with reg.
func NewChecker(prom Querier, checks []Check, ttl time.Duration, reg prometheus.Registerer) *Checker {
	c := &Checker{
		prom:    prom,
		checks:  checks,
		ttl:     ttl,
		timeout: 10 * time.Second,
		now:     time.Now,
		queryErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cyberlab_status_query_errors_total",
			Help: "Prometheus queries that failed, by check.",
		}, []string{"check"}),
		refreshDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "cyberlab_status_refresh_duration_seconds",
			Help:    "Time to run every check against Prometheus.",
			Buckets: prometheus.DefBuckets,
		}),
		checkLevel: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cyberlab_status_check_level",
			Help: "Level of each check at the last refresh: 0 ok, 1 warn, 2 crit, 3 unknown.",
		}, []string{"check"}),
	}
	reg.MustRegister(c.queryErrors, c.refreshDuration, c.checkLevel)
	return c
}

// Snapshot returns the cached snapshot, refreshing it first if it's older than ttl.
// Holding the lock during the refresh means concurrent requests wait for one
// refresh instead of each starting their own.
func (c *Checker) Snapshot(ctx context.Context) Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cached != nil && c.now().Sub(c.cached.CheckedAt) < c.ttl {
		return *c.cached
	}
	s := c.refresh(ctx)
	c.cached = &s
	return s
}

// Run refreshes the snapshot every ttl until ctx is cancelled. Without it the checks
// only run when someone loads the page, so the app's own metrics (and alerts built
// on them) would go stale whenever nobody is looking.
func (c *Checker) Run(ctx context.Context) {
	t := time.NewTicker(c.ttl)
	defer t.Stop()
	for {
		c.mu.Lock()
		s := c.refresh(ctx)
		c.cached = &s
		c.mu.Unlock()

		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (c *Checker) refresh(ctx context.Context) Snapshot {
	start := c.now()
	// Not the request's context: a refresh shared by every waiting request
	// shouldn't be cut short because the first browser tab closed.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.timeout)
	defer cancel()

	results := make([]Result, len(c.checks))
	var alerts []Alert
	var alertsErr error

	var wg sync.WaitGroup
	for i, check := range c.checks {
		wg.Go(func() {
			results[i] = c.run(ctx, check)
		})
	}
	wg.Go(func() {
		alerts, alertsErr = c.firingAlerts(ctx)
	})
	wg.Wait()

	overall := OK
	for _, r := range results {
		overall = max(overall, r.Level)
		c.checkLevel.WithLabelValues(r.ID).Set(float64(r.Level))
	}
	if alertsErr != nil {
		slog.Warn("listing firing alerts failed", "err", alertsErr)
		c.queryErrors.WithLabelValues("firing_alerts").Inc()
	}
	for _, a := range alerts {
		if a.Severity == "critical" {
			overall = max(overall, Crit)
		} else {
			overall = max(overall, Warn)
		}
	}

	c.refreshDuration.Observe(c.now().Sub(start).Seconds())
	return Snapshot{Overall: overall, Results: results, Alerts: alerts, CheckedAt: c.now()}
}

func (c *Checker) run(ctx context.Context, check Check) Result {
	v, err := c.prom.QueryValue(ctx, check.Query)
	if err != nil {
		slog.Warn("check failed", "check", check.ID, "err", err)
		c.queryErrors.WithLabelValues(check.ID).Inc()
		return Result{Check: check, Level: Unknown, Error: "couldn't query Prometheus"}
	}
	return Result{Check: check, Value: v, Level: check.Evaluate(v)}
}

func (c *Checker) firingAlerts(ctx context.Context) ([]Alert, error) {
	samples, err := c.prom.Query(ctx, firingAlerts)
	if err != nil {
		return nil, err
	}
	alerts := make([]Alert, 0, len(samples))
	for _, s := range samples {
		alerts = append(alerts, Alert{
			Name:      s.Labels["alertname"],
			Severity:  s.Labels["severity"],
			Namespace: s.Labels["namespace"],
		})
	}
	// Critical first, then by name, so the page doesn't reshuffle on every refresh
	slices.SortFunc(alerts, func(a, b Alert) int {
		if (a.Severity == "critical") != (b.Severity == "critical") {
			if a.Severity == "critical" {
				return -1
			}
			return 1
		}
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.Namespace, b.Namespace))
	})
	return alerts, nil
}
