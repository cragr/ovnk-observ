// Package incengine parses the ovn-northd "inc-engine/show-stats" reply and
// exposes the allowlisted engine-node counters as Prometheus metrics.
package incengine

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cragr/ovnk-observ/pkg/appctl"
	"github.com/prometheus/client_golang/prometheus"
)

// Stats holds the run counters of one incremental-engine node.
type Stats struct{ Recompute, Compute, Cancel uint64 }

// DefaultNodes is the default allowlist of engine nodes to export.
var DefaultNodes = []string{"northd", "lflow", "port_group", "sync_to_sb_addr_set", "sync_from_sb", "ls_stateful", "lr_stateful"}

// Parse reads show-stats output ("Node: <name>" followed by "- <stat>: <n>"
// lines) into per-node counters. Unknown stats, non-numeric values and lines
// outside a node block are ignored.
func Parse(s string) map[string]Stats {
	out := map[string]Stats{}
	cur := ""
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if name, ok := strings.CutPrefix(line, "Node:"); ok {
			cur = strings.TrimSpace(name)
			if cur != "" {
				if _, seen := out[cur]; !seen {
					out[cur] = Stats{}
				}
			}
			continue
		}
		rest, ok := strings.CutPrefix(line, "-")
		if !ok || cur == "" {
			continue
		}
		key, val, ok := strings.Cut(rest, ":")
		if !ok {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(val), 10, 64)
		if err != nil {
			continue
		}
		st := out[cur]
		switch strings.TrimSpace(key) {
		case "recompute":
			st.Recompute = n
		case "compute":
			st.Compute = n
		case "cancel":
			st.Cancel = n
		default:
			continue
		}
		out[cur] = st
	}
	return out
}

type collector struct {
	fetch   func(context.Context) (string, error)
	nodes   []string
	timeout time.Duration

	runs, errs *prometheus.Desc

	mu       sync.Mutex
	last     map[string]Stats // last good result
	lastOK   bool             // whether the previous scrape fetched successfully
	errCount float64
}

// NewCollector builds a collector that calls fetch on each scrape and emits
// run counters for the allowlisted nodes that are present. After a failed
// fetch the last good result is emitted once more; further consecutive
// failures emit no run counters.
func NewCollector(fetch func(context.Context) (string, error), nodes []string, timeout time.Duration) prometheus.Collector {
	return &collector{
		fetch: fetch, nodes: nodes, timeout: timeout,
		runs: prometheus.NewDesc("ovn_northd_inc_engine_runs_total",
			"Total northd incremental-engine node runs by type.", []string{"engine_node", "type"}, nil),
		errs: prometheus.NewDesc("ovnk_observ_appctl_errors_total",
			"Total failed ovn-appctl requests by command.", []string{"command"}, nil),
	}
}

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.runs
	ch <- c.errs
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	// Prometheus may scrape concurrently; serialize so the cache stays consistent.
	c.mu.Lock()
	defer c.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	var emit map[string]Stats
	if text, err := c.fetch(ctx); err != nil {
		c.errCount++
		if c.lastOK {
			emit = c.last
		}
		c.lastOK = false
	} else {
		c.last = Parse(text)
		c.lastOK = true
		emit = c.last
	}

	for _, n := range c.nodes {
		st, ok := emit[n]
		if !ok {
			continue
		}
		for _, v := range []struct {
			typ string
			n   uint64
		}{{"recompute", st.Recompute}, {"compute", st.Compute}, {"cancel", st.Cancel}} {
			ch <- prometheus.MustNewConstMetric(c.runs, prometheus.CounterValue, float64(v.n), n, v.typ)
		}
	}
	ch <- prometheus.MustNewConstMetric(c.errs, prometheus.CounterValue, c.errCount, appctl.ShowStatsCommand)
}
