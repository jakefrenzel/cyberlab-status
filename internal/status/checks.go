package status

// Level is how worrying a check's value is. Higher is worse, so the overall status
// of the page is simply the highest level of any check.
type Level int

const (
	OK Level = iota
	Warn
	Crit
	// Unknown means the check couldn't run (Prometheus unreachable, bad query).
	Unknown
)

func (l Level) String() string {
	switch l {
	case OK:
		return "ok"
	case Warn:
		return "warn"
	case Crit:
		return "crit"
	default:
		return "unknown"
	}
}

// MarshalText makes levels show up as "ok"/"warn"/... in the JSON API.
func (l Level) MarshalText() ([]byte, error) { return []byte(l.String()), nil }

// Check is one number on the page. Every check counts problems, so zero is healthy.
type Check struct {
	ID      string `json:"id"`
	Section string `json:"section"`
	Name    string `json:"name"`
	Help    string `json:"help"`
	Query   string `json:"-"`
	// Warn and Crit are the values at which the check turns amber or red. Zero
	// disables a threshold (every value is >= 0, so it would always trigger).
	Warn float64 `json:"-"`
	Crit float64 `json:"-"`
}

// Evaluate maps a value onto a level using the check's thresholds.
func (c Check) Evaluate(v float64) Level {
	switch {
	case c.Crit > 0 && v >= c.Crit:
		return Crit
	case c.Warn > 0 && v >= c.Warn:
		return Warn
	default:
		return OK
	}
}

// Sections in the order they appear on the page.
var Sections = []string{"Cluster", "GitOps", "Network", "Security"}

// falcoEvents24h counts Falco rule matches in the last 24 hours, including each
// rule's first match on a node. increase() would miss those: the counter only
// appears when a rule first fires, so it has no earlier sample to compare to.
const falcoEvents24h = `round(sum(clamp_min(` +
	`falcosecurity_falco_rules_matches_total{priority=~"[0-4]"} - ` +
	`(falcosecurity_falco_rules_matches_total{priority=~"[0-4]"} offset 24h), 0) ` +
	`or falcosecurity_falco_rules_matches_total{priority=~"[0-4]"})) or vector(0)`

// DefaultChecks mirror the counts on the Cyberlab Overview Grafana dashboard.
var DefaultChecks = []Check{
	{
		ID: "nodes_not_ready", Section: "Cluster", Name: "Nodes not ready",
		Help:  "Nodes whose Ready condition isn't true.",
		Query: `count(kube_node_info) - sum(kube_node_status_condition{condition="Ready",status="true"})`,
		Crit:  1,
	},
	{
		ID: "pods_not_running", Section: "Cluster", Name: "Pods not running",
		Help:  "Pods stuck in Pending, Failed, or Unknown.",
		Query: `sum(kube_pod_status_phase{phase=~"Pending|Failed|Unknown"}) or vector(0)`,
		Warn:  1, Crit: 3,
	},
	{
		ID: "restarts_1h", Section: "Cluster", Name: "Container restarts (1h)",
		Help:  "Restarts in the last hour. One or two after a rollout is normal; a climb is a crash loop.",
		Query: `round(sum(increase(kube_pod_container_status_restarts_total[1h]))) or vector(0)`,
		Warn:  1, Crit: 5,
	},
	{
		ID: "volumes_full", Section: "Cluster", Name: "Volumes over 80%",
		Help:  "Persistent volumes more than 80% full.",
		Query: `count(kubelet_volume_stats_used_bytes / kubelet_volume_stats_capacity_bytes > 0.8) or vector(0)`,
		Warn:  1,
	},
	{
		ID: "apps_out_of_sync", Section: "GitOps", Name: "Apps out of sync",
		Help:  "Argo CD apps that don't match main. Auto-sync usually fixes this within minutes.",
		Query: `count(argocd_app_info{sync_status!="Synced"}) or vector(0)`,
		Warn:  1,
	},
	{
		ID: "apps_unhealthy", Section: "GitOps", Name: "Apps unhealthy",
		Help:  "Argo CD apps whose resources aren't healthy.",
		Query: `count(argocd_app_info{health_status!="Healthy"}) or vector(0)`,
		Warn:  1,
	},
	{
		ID: "targets_down", Section: "Network", Name: "Scrape targets down",
		Help:  "Things Prometheus can't reach.",
		Query: `count(up == 0) or vector(0)`,
		Crit:  1,
	},
	{
		ID: "policy_drops_1h", Section: "Network", Name: "Policy drops (1h)",
		Help:  "Packets dropped by the host firewall and NetworkPolicies. A few is background; hundreds is something knocking.",
		Query: `round(sum(increase(cilium_drop_count_total{reason="Policy denied"}[1h]))) or vector(0)`,
		Warn:  100,
	},
	{
		ID: "falco_events_24h", Section: "Security", Name: "Falco events (24h)",
		Help:  "Falco detections at Warning or above, every node.",
		Query: falcoEvents24h,
		Warn:  1, Crit: 20,
	},
	{
		ID: "falco_nodes_down", Section: "Security", Name: "Falco nodes not reporting",
		Help:  "Nodes without a healthy Falco.",
		Query: `count(kube_node_info) - (count(up{job="falco-metrics"} == 1) or vector(0))`,
		Crit:  1,
	},
	{
		ID: "syscall_drops_1h", Section: "Security", Name: "Syscalls dropped (1h)",
		Help:  "Events Falco couldn't keep up with: blind spots.",
		Query: `round(sum(increase(falcosecurity_scap_n_drops_total[1h]))) or vector(0)`,
		Warn:  1, Crit: 1000,
	},
}

// firingAlerts lists alerts that would notify someone (the always-on Watchdog
// heartbeat and info-level alerts excluded).
const firingAlerts = `ALERTS{alertstate="firing",alertname!~"Watchdog|InfoInhibitor",severity!~"none|info"}`
