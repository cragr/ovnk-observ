// Package k8scount exports counts of cluster-scoped Kubernetes objects
// (NADs, MultiNetworkPolicies, NetworkPolicies, UDNs/CUDNs) computed from
// informer caches at scrape time. Annotation contents are never exported;
// only the number of policy targets is.
package k8scount

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

const policyForAnnotation = "k8s.v1.cni.cncf.io/policy-for"

var (
	nadGVR  = schema.GroupVersionResource{Group: "k8s.cni.cncf.io", Version: "v1", Resource: "network-attachment-definitions"}
	mnpGVR  = schema.GroupVersionResource{Group: "k8s.cni.cncf.io", Version: "v1beta1", Resource: "multi-networkpolicies"}
	udnGVR  = schema.GroupVersionResource{Group: "k8s.ovn.org", Version: "v1", Resource: "userdefinednetworks"}
	cudnGVR = schema.GroupVersionResource{Group: "k8s.ovn.org", Version: "v1", Resource: "clusteruserdefinednetworks"}
)

// resourceNames is the fixed set of values of the informer_synced "resource" label.
var resourceNames = []string{"nad", "mnp", "networkpolicy", "udn", "cudn"}

type collector struct {
	// informers maps resource label to its informer; nil when the resource
	// is not served by the cluster.
	informers map[string]cache.SharedIndexInformer

	churn *churn

	nads, mnps, mnpTargets, mnpRules, mnpPeers, nps, udns, synced, events *prometheus.Desc
}

// NewCollector starts informers and returns immediately. Dynamic resources
// are watched only if discovery reports them; a missing CRD leaves that
// resource unsynced without error. It also counts informer add/update/delete
// events (never the initial list) in the churn counter.
func NewCollector(ctx context.Context, dyn dynamic.Interface, kube kubernetes.Interface, opts Options) (prometheus.Collector, error) {
	c := &collector{
		informers: map[string]cache.SharedIndexInformer{},
		churn:     newChurn(opts.ManagerLabels),
		events: prometheus.NewDesc(churnMetricName,
			"Informer add/update/delete events by resource, op and last-writing manager.", []string{"resource", "op", "manager"}, nil),
		nads: prometheus.NewDesc("ovnkube_clustermanager_network_attachment_definitions",
			"Number of NetworkAttachmentDefinitions.", []string{"namespace", "managed_by"}, nil),
		mnps: prometheus.NewDesc("ovnkube_clustermanager_multi_network_policies",
			"Number of MultiNetworkPolicies.", []string{"namespace"}, nil),
		mnpTargets: prometheus.NewDesc("ovnkube_clustermanager_multi_network_policy_network_targets",
			"Number of networks targeted by MultiNetworkPolicies (policy-for entries).", []string{"namespace"}, nil),
		mnpRules: prometheus.NewDesc("ovnkube_clustermanager_multi_network_policy_rules",
			"Number of ingress/egress rules across MultiNetworkPolicies.", []string{"namespace", "direction"}, nil),
		mnpPeers: prometheus.NewDesc("ovnkube_clustermanager_multi_network_policy_peers",
			"Number of from/to peers across MultiNetworkPolicy rules.", []string{"namespace", "direction"}, nil),
		nps: prometheus.NewDesc("ovnkube_clustermanager_network_policies",
			"Number of NetworkPolicies.", []string{"namespace"}, nil),
		udns: prometheus.NewDesc("ovnkube_clustermanager_user_defined_networks",
			"Number of UserDefinedNetworks and ClusterUserDefinedNetworks.", []string{"kind", "topology", "role"}, nil),
		synced: prometheus.NewDesc("ovnk_observ_informer_synced",
			"Whether the informer for a resource has completed its initial sync.", []string{"resource"}, nil),
	}

	np := informers.NewSharedInformerFactory(kube, 0)
	c.informers["networkpolicy"] = np.Networking().V1().NetworkPolicies().Informer()
	transforms := map[string]cache.TransformFunc{
		"networkpolicy": transformNetworkPolicy,
		"nad":           transformNAD, "mnp": transformMNP,
		"udn": transformUDN, "cudn": transformCUDN,
	}

	factory := dynamicinformer.NewDynamicSharedInformerFactory(dyn, 0)
	gvCache := map[string]map[string]bool{}
	available := func(gvr schema.GroupVersionResource) bool {
		gv := gvr.GroupVersion().String()
		names, ok := gvCache[gv]
		if !ok {
			names = map[string]bool{}
			list, err := kube.Discovery().ServerResourcesForGroupVersion(gv)
			if err != nil {
				slog.Warn("resource discovery failed; not watching", "groupVersion", gv, "error", err)
			} else if list != nil {
				for _, r := range list.APIResources {
					names[r.Name] = true
				}
			}
			gvCache[gv] = names
		}
		if !names[gvr.Resource] {
			slog.Info("resource not available; not watching", "resource", gvr.String())
			return false
		}
		return true
	}
	for _, d := range []struct {
		name string
		gvr  schema.GroupVersionResource
	}{{"nad", nadGVR}, {"mnp", mnpGVR}, {"udn", udnGVR}, {"cudn", cudnGVR}} {
		if available(d.gvr) {
			c.informers[d.name] = factory.ForResource(d.gvr).Informer()
		}
	}

	// Transforms must be set before the informers start.
	for name, inf := range c.informers {
		if err := inf.SetTransform(transforms[name]); err != nil {
			return nil, fmt.Errorf("set %s informer transform: %w", name, err)
		}
	}

	// Handlers go on before the factories start so no event is missed.
	for name, inf := range c.informers {
		if _, err := inf.AddEventHandler(c.churn.handler(name)); err != nil {
			return nil, fmt.Errorf("add %s event handler: %w", name, err)
		}
	}

	np.Start(ctx.Done())
	factory.Start(ctx.Done())
	return c, nil
}

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.nads, c.mnps, c.mnpTargets, c.mnpRules, c.mnpPeers, c.nps, c.udns, c.synced, c.events} {
		ch <- d
	}
}

// list returns the cached objects of a resource, or ok=false if it is
// absent or not yet synced.
func (c *collector) list(name string) ([]interface{}, bool) {
	inf := c.informers[name]
	if inf == nil || !inf.HasSynced() {
		return nil, false
	}
	return inf.GetStore().List(), true
}

// annotationCount parses a decimal count annotation; missing or invalid is 0.
func annotationCount(v string) float64 {
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	return float64(n)
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	c.churn.collect(ch, c.events)
	for _, r := range resourceNames {
		v := 0.0
		if inf := c.informers[r]; inf != nil && inf.HasSynced() {
			v = 1
		}
		ch <- prometheus.MustNewConstMetric(c.synced, prometheus.GaugeValue, v, r)
	}

	if objs, ok := c.list("nad"); ok {
		type key struct{ ns, by string }
		counts := map[key]float64{}
		for _, o := range objs {
			if u, ok := o.(*unstructured.Unstructured); ok {
				counts[key{u.GetNamespace(), managedBy(u)}]++
			}
		}
		for k, n := range counts {
			ch <- prometheus.MustNewConstMetric(c.nads, prometheus.GaugeValue, n, k.ns, k.by)
		}
	}

	if objs, ok := c.list("mnp"); ok {
		pols, targets := map[string]float64{}, map[string]float64{}
		type dkey struct{ ns, dir string }
		rules, peers := map[dkey]float64{}, map[dkey]float64{}
		for _, o := range objs {
			if u, ok := o.(*unstructured.Unstructured); ok {
				ns := u.GetNamespace()
				pols[ns]++
				targets[ns] += float64(countPolicyTargets(u.GetAnnotations()[policyForAnnotation]))
				ann := u.GetAnnotations()
				for _, d := range []struct{ dir, rk, pk string }{
					{"ingress", ingressRulesAnnotation, ingressPeersAnnotation},
					{"egress", egressRulesAnnotation, egressPeersAnnotation},
				} {
					k := dkey{ns, d.dir}
					rules[k] += annotationCount(ann[d.rk])
					peers[k] += annotationCount(ann[d.pk])
				}
			}
		}
		for ns, n := range pols {
			ch <- prometheus.MustNewConstMetric(c.mnps, prometheus.GaugeValue, n, ns)
			ch <- prometheus.MustNewConstMetric(c.mnpTargets, prometheus.GaugeValue, targets[ns], ns)
			for _, dir := range []string{"ingress", "egress"} {
				k := dkey{ns, dir}
				ch <- prometheus.MustNewConstMetric(c.mnpRules, prometheus.GaugeValue, rules[k], ns, dir)
				ch <- prometheus.MustNewConstMetric(c.mnpPeers, prometheus.GaugeValue, peers[k], ns, dir)
			}
		}
	}

	if objs, ok := c.list("networkpolicy"); ok {
		counts := map[string]float64{}
		for _, o := range objs {
			if p, ok := o.(*networkingv1.NetworkPolicy); ok {
				counts[p.Namespace]++
			}
		}
		for ns, n := range counts {
			ch <- prometheus.MustNewConstMetric(c.nps, prometheus.GaugeValue, n, ns)
		}
	}

	type ukey struct{ kind, topo, role string }
	ucounts := map[ukey]float64{}
	for _, d := range []struct {
		res, kind string
		path      []string
	}{{"udn", "UDN", []string{"spec"}}, {"cudn", "CUDN", []string{"spec", "network"}}} {
		objs, ok := c.list(d.res)
		if !ok {
			continue
		}
		for _, o := range objs {
			if u, ok := o.(*unstructured.Unstructured); ok {
				topo, role := udnShape(u, d.path)
				ucounts[ukey{d.kind, topo, role}]++
			}
		}
	}
	for k, n := range ucounts {
		ch <- prometheus.MustNewConstMetric(c.udns, prometheus.GaugeValue, n, k.kind, k.topo, k.role)
	}
}

func managedBy(u *unstructured.Unstructured) string {
	for _, o := range u.GetOwnerReferences() {
		if strings.HasPrefix(o.APIVersion, "k8s.ovn.org/") {
			return "udn"
		}
	}
	return "user"
}

// countPolicyTargets counts non-empty comma-separated entries.
func countPolicyTargets(s string) int {
	n := 0
	for _, e := range strings.Split(s, ",") {
		if strings.TrimSpace(e) != "" {
			n++
		}
	}
	return n
}

func udnShape(u *unstructured.Unstructured, base []string) (topology, role string) {
	topology, _, _ = unstructured.NestedString(u.Object, append(append([]string{}, base...), "topology")...)
	for _, l := range []string{"layer2", "layer3"} {
		if r, found, _ := unstructured.NestedString(u.Object, append(append([]string{}, base...), l, "role")...); found {
			return topology, r
		}
	}
	return topology, ""
}
