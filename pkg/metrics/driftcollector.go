package metrics

import "github.com/prometheus/client_golang/prometheus"

// DriftSource reports NB/SB Port_Group drift counts.
type DriftSource interface {
	Missing() (missing, withPorts int)
}

type driftCollector struct {
	src              DriftSource
	nbState, sbState *DBState

	missing, withPorts *prometheus.Desc
}

// NewDriftCollector builds a collector for Port_Group drift. It emits nothing
// until both databases have completed their initial sync, because a partial
// view would read as drift.
func NewDriftCollector(src DriftSource, nbState, sbState *DBState) prometheus.Collector {
	return &driftCollector{
		src: src, nbState: nbState, sbState: sbState,
		missing: prometheus.NewDesc("ovnkube_controller_port_group_sb_missing",
			"Northbound Port_Groups holding ports that are absent from the southbound database.", nil, nil),
		withPorts: prometheus.NewDesc("ovnkube_controller_port_group_with_ports",
			"Northbound Port_Groups holding at least one port.", nil, nil),
	}
}

func (c *driftCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.missing
	ch <- c.withPorts
}

func (c *driftCollector) Collect(ch chan<- prometheus.Metric) {
	nbConn, _, _ := c.nbState.get()
	sbConn, _, _ := c.sbState.get()
	if !nbConn || !sbConn {
		return
	}
	missing, withPorts := c.src.Missing()
	ch <- prometheus.MustNewConstMetric(c.missing, prometheus.GaugeValue, float64(missing))
	ch <- prometheus.MustNewConstMetric(c.withPorts, prometheus.GaugeValue, float64(withPorts))
}
