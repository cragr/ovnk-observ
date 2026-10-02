package metrics

import (
	"sync"
	"time"

	"github.com/cragr/ovnk-observ/pkg/nbcount"
	"github.com/prometheus/client_golang/prometheus"
)

// DBState tracks connection state of one OVSDB connection. Safe for
// concurrent use.
type DBState struct {
	mu          sync.Mutex
	connected   bool
	synced      bool
	initialSync time.Duration
}

// Set records a connection event. A true call records the initial sync
// duration; the last value is kept across disconnects.
func (s *DBState) Set(connected bool, initialSync time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connected = connected
	if connected {
		s.synced = true
		s.initialSync = initialSync
	}
}

func (s *DBState) get() (connected, synced bool, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connected, s.synced, s.initialSync
}

type nodeCollector struct {
	nb, sb           *nbcount.Counter
	nbState, sbState *DBState
	mode             NetworkLabelMode

	nbObjects, sbObjects, updates, connected, initialSync *prometheus.Desc
}

// NewNodeCollector builds a collector that renders counter snapshots on each scrape.
func NewNodeCollector(nb, sb *nbcount.Counter, nbState, sbState *DBState, mode NetworkLabelMode) prometheus.Collector {
	return &nodeCollector{
		nb: nb, sb: sb, nbState: nbState, sbState: sbState, mode: mode,
		nbObjects: prometheus.NewDesc("ovnkube_controller_nb_db_objects",
			"Number of rows in the OVN northbound database.", []string{"table", "owner_type", "network"}, nil),
		sbObjects: prometheus.NewDesc("ovnkube_controller_sb_db_objects",
			"Number of rows in the OVN southbound database.", []string{"table"}, nil),
		updates: prometheus.NewDesc("ovnkube_controller_nb_db_updates_total",
			"Total number of northbound database row updates seen.", []string{"table", "op"}, nil),
		connected: prometheus.NewDesc("ovnk_observ_db_connected",
			"Whether the exporter is connected to the OVN database.", []string{"db"}, nil),
		initialSync: prometheus.NewDesc("ovnk_observ_initial_sync_seconds",
			"Duration of the last initial database sync in seconds.", []string{"db"}, nil),
	}
}

func (c *nodeCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.nbObjects, c.sbObjects, c.updates, c.connected, c.initialSync} {
		ch <- d
	}
}

func (c *nodeCollector) Collect(ch chan<- prometheus.Metric) {
	nbConn := false
	sbConn := false
	for _, db := range []struct {
		name  string
		state *DBState
		conn  *bool
	}{{"nb", c.nbState, &nbConn}, {"sb", c.sbState, &sbConn}} {
		conn, synced, d := db.state.get()
		*db.conn = conn
		v := 0.0
		if conn {
			v = 1
		}
		ch <- prometheus.MustNewConstMetric(c.connected, prometheus.GaugeValue, v, db.name)
		if synced {
			ch <- prometheus.MustNewConstMetric(c.initialSync, prometheus.GaugeValue, d.Seconds(), db.name)
		}
	}

	// Cumulative; emitted regardless of connection state.
	for k, n := range c.nb.Updates() {
		ch <- prometheus.MustNewConstMetric(c.updates, prometheus.CounterValue, float64(n), k[0], k[1])
	}

	if nbConn {
		for table, keys := range c.nb.Snapshot() {
			c.emitNB(ch, table, keys)
		}
	}
	if sbConn {
		for table, keys := range c.sb.Snapshot() {
			sum := 0
			for _, n := range keys {
				sum += n
			}
			ch <- prometheus.MustNewConstMetric(c.sbObjects, prometheus.GaugeValue, float64(sum), table)
		}
	}
}

func (c *nodeCollector) emitNB(ch chan<- prometheus.Metric, table string, keys map[nbcount.Key]int) {
	byOwner := map[string]map[string]int{} // owner_type -> network -> count
	for k, n := range keys {
		net := k.Network
		if c.mode.Off {
			net = ""
		}
		if byOwner[k.OwnerType] == nil {
			byOwner[k.OwnerType] = map[string]int{}
		}
		byOwner[k.OwnerType][net] += n
	}
	for owner, nets := range byOwner {
		if !c.mode.Off {
			nets = FoldTopN(nets, c.mode.TopN)
		}
		for net, n := range nets {
			ch <- prometheus.MustNewConstMetric(c.nbObjects, prometheus.GaugeValue, float64(n), table, owner, net)
		}
	}
}
