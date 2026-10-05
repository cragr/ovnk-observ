package k8scount

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/tools/cache"
)

const (
	churnMetricName = "ovnkube_clustermanager_object_events_total"

	// unknownManager labels objects with no managedFields and delete events.
	unknownManager = "unknown"
	// otherManager collects every manager beyond the first N.
	otherManager = "_other"
)

// Options configures NewCollector.
type Options struct {
	// ManagerLabels is the number of distinct managers that keep their own
	// "manager" label value on the churn counter; later ones count under
	// "_other". 0 turns the label off (empty value).
	ManagerLabels int
}

// churn counts informer events by resource, op and manager. A counter
// series cannot move between label values without breaking rate(), so the
// first N distinct managers keep their label for the life of the process
// and every later manager folds into "_other".
type churn struct {
	max int

	mu     sync.Mutex
	seen   map[string]bool
	counts map[churnKey]float64
}

type churnKey struct{ resource, op, manager string }

func newChurn(maxManagers int) *churn {
	return &churn{max: maxManagers, seen: map[string]bool{}, counts: map[churnKey]float64{}}
}

// record counts one event.
func (c *churn) record(resource, op, manager string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case c.max <= 0:
		manager = ""
	case c.seen[manager]:
	case len(c.seen) < c.max:
		c.seen[manager] = true
	default:
		manager = otherManager
	}
	c.counts[churnKey{resource, op, manager}]++
}

// collect emits one counter per series.
func (c *churn) collect(ch chan<- prometheus.Metric, desc *prometheus.Desc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, n := range c.counts {
		ch <- prometheus.MustNewConstMetric(desc, prometheus.CounterValue, n, k.resource, k.op, k.manager)
	}
}

// handler returns informer event handlers recording into c. Handlers see
// transformed objects, so the manager comes from the private annotation.
func (c *churn) handler(resource string) cache.ResourceEventHandlerDetailedFuncs {
	return cache.ResourceEventHandlerDetailedFuncs{
		AddFunc: func(obj interface{}, isInInitialList bool) {
			if isInInitialList {
				return
			}
			c.record(resource, "add", managerOf(obj))
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			o, errO := meta.Accessor(oldObj)
			n, errN := meta.Accessor(newObj)
			if errO != nil || errN != nil || o.GetResourceVersion() == n.GetResourceVersion() {
				return
			}
			c.record(resource, "update", managerOf(newObj))
		},
		DeleteFunc: func(obj interface{}) {
			// The cached object names the last writer, not the deleter.
			c.record(resource, "delete", unknownManager)
		},
	}
}

// managerOf reads the private manager annotation.
func managerOf(obj interface{}) string {
	if m, err := meta.Accessor(obj); err == nil {
		if v := m.GetAnnotations()[managerAnnotation]; v != "" {
			return v
		}
	}
	return unknownManager
}
