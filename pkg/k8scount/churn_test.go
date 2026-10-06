package k8scount

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

var testChurnDesc = prometheus.NewDesc(churnMetricName, "test", []string{"resource", "op", "manager"}, nil)

type churnWrap struct{ c *churn }

func (w churnWrap) Describe(ch chan<- *prometheus.Desc) { ch <- testChurnDesc }
func (w churnWrap) Collect(ch chan<- prometheus.Metric) { w.c.collect(ch, testChurnDesc) }

func compareChurn(t *testing.T, c *churn, body string) {
	t.Helper()
	exp := "# HELP " + churnMetricName + " test\n# TYPE " + churnMetricName + " counter\n" + body
	if err := testutil.CollectAndCompare(churnWrap{c}, strings.NewReader(exp), churnMetricName); err != nil {
		t.Fatal(err)
	}
}

func TestChurnFirstNManagersStable(t *testing.T) {
	c := newChurn(2)
	for _, m := range []string{"a", "b", "c", "a"} {
		c.record("mnp", "update", m)
	}
	compareChurn(t, c, `
ovnkube_clustermanager_object_events_total{manager="_other",op="update",resource="mnp"} 1
ovnkube_clustermanager_object_events_total{manager="a",op="update",resource="mnp"} 2
ovnkube_clustermanager_object_events_total{manager="b",op="update",resource="mnp"} 1
`)
	c.record("mnp", "update", "d")
	compareChurn(t, c, `
ovnkube_clustermanager_object_events_total{manager="_other",op="update",resource="mnp"} 2
ovnkube_clustermanager_object_events_total{manager="a",op="update",resource="mnp"} 2
ovnkube_clustermanager_object_events_total{manager="b",op="update",resource="mnp"} 1
`)
}

func TestChurnOffDropsManager(t *testing.T) {
	c := newChurn(0)
	c.record("nad", "add", "a")
	c.record("nad", "add", "b")
	compareChurn(t, c, `
ovnkube_clustermanager_object_events_total{manager="",op="add",resource="nad"} 2
`)
}

func TestChurnSkipsUnchangedResourceVersion(t *testing.T) {
	c := newChurn(10)
	h := c.handler("mnp")
	mk := func(rv string) *metav1.PartialObjectMetadata {
		o := &metav1.PartialObjectMetadata{}
		o.ResourceVersion = rv
		o.Annotations = map[string]string{managerAnnotation: "m"}
		return o
	}
	h.UpdateFunc(mk("1"), mk("1"))
	compareChurn(t, c, "")
	h.UpdateFunc(mk("1"), mk("2"))
	compareChurn(t, c, `
ovnkube_clustermanager_object_events_total{manager="m",op="update",resource="mnp"} 1
`)
	// Deletes ignore the object, so a tombstone counts the same as a plain
	// delete, always under manager="unknown".
	h.DeleteFunc(cache.DeletedFinalStateUnknown{Key: "ns/x", Obj: mk("3")})
	h.DeleteFunc(mk("3"))
	compareChurn(t, c, `
ovnkube_clustermanager_object_events_total{manager="m",op="update",resource="mnp"} 1
ovnkube_clustermanager_object_events_total{manager="unknown",op="delete",resource="mnp"} 2
`)
}

func TestChurnSkipsInitialList(t *testing.T) {
	pre := obj(mnpGVK, "ns1", "pre", nil)
	c, dyn := newCollectorOpts(t, Options{ManagerLabels: 10}, allGVRs, nil, pre)
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, ok := c.(*collector).list("mnp"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("mnp informer did not sync")
		}
	}
	// Nothing recorded for the pre-existing object.
	if n := testutil.CollectAndCount(c, churnMetricName); n != 0 {
		t.Fatalf("initial list produced %d churn series", n)
	}
	post := obj(mnpGVK, "ns1", "post", nil)
	if _, err := dyn.Resource(mnpGVR).Namespace("ns1").Create(context.Background(), post, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	// The fake rejects managedFields on create, so the manager is "unknown".
	eventually(t, c, `
# HELP ovnkube_clustermanager_object_events_total Informer add/update/delete events by resource, op and last-writing manager.
# TYPE ovnkube_clustermanager_object_events_total counter
ovnkube_clustermanager_object_events_total{manager="unknown",op="add",resource="mnp"} 1
`, churnMetricName)
}

func TestChurnUnknownBypassesSlots(t *testing.T) {
	c := newChurn(1)
	c.record("mnp", "update", "a")
	c.record("mnp", "delete", unknownManager)
	c.record("mnp", "update", "b")
	c.record("mnp", "update", unknownManager)
	compareChurn(t, c, `
ovnkube_clustermanager_object_events_total{manager="_other",op="update",resource="mnp"} 1
ovnkube_clustermanager_object_events_total{manager="a",op="update",resource="mnp"} 1
ovnkube_clustermanager_object_events_total{manager="unknown",op="delete",resource="mnp"} 1
ovnkube_clustermanager_object_events_total{manager="unknown",op="update",resource="mnp"} 1
`)
	// With off, the label stays empty even for unknown.
	o := newChurn(0)
	o.record("mnp", "delete", unknownManager)
	compareChurn(t, o, `
ovnkube_clustermanager_object_events_total{manager="",op="delete",resource="mnp"} 1
`)
}
