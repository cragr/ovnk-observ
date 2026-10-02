package k8scount

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
)

var allGVRs = []schema.GroupVersionResource{nadGVR, mnpGVR, udnGVR, cudnGVR}

func obj(gvk schema.GroupVersionKind, ns, name string, extra map[string]interface{}) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{}}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace(ns)
	u.SetName(name)
	for k, v := range extra {
		u.Object[k] = v
	}
	return u
}

func newCollector(t *testing.T, existing []schema.GroupVersionResource, np []*networkingv1.NetworkPolicy, objs ...runtime.Object) prometheus.Collector {
	t.Helper()
	kinds := map[schema.GroupVersionResource]string{
		nadGVR: "NetworkAttachmentDefinitionList", mnpGVR: "MultiNetworkPolicyList",
		udnGVR: "UserDefinedNetworkList", cudnGVR: "ClusterUserDefinedNetworkList",
	}
	lk := map[schema.GroupVersionResource]string{}
	byGV := map[string][]metav1.APIResource{}
	for _, g := range existing {
		lk[g] = kinds[g]
		byGV[g.GroupVersion().String()] = append(byGV[g.GroupVersion().String()], metav1.APIResource{Name: g.Resource})
	}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), lk)
	// The fake tracker guesses plurals from Kind, which does not match
	// hyphenated resource names, so create objects through the real GVR.
	for _, o := range objs {
		u := o.(*unstructured.Unstructured)
		var gvr schema.GroupVersionResource
		for _, g := range existing {
			if g.Group == u.GroupVersionKind().Group && kinds[g] == u.GetKind()+"List" {
				gvr = g
			}
		}
		if gvr.Empty() {
			continue
		}
		if _, err := dyn.Resource(gvr).Namespace(u.GetNamespace()).Create(context.Background(), u, metav1.CreateOptions{}); err != nil {
			t.Fatalf("create %s: %v", u.GetName(), err)
		}
	}
	var kobjs []runtime.Object
	for _, p := range np {
		kobjs = append(kobjs, p)
	}
	kube := kubefake.NewSimpleClientset(kobjs...)
	fd := kube.Discovery().(*fakediscovery.FakeDiscovery)
	for gv, rs := range byGV {
		fd.Resources = append(fd.Resources, &metav1.APIResourceList{GroupVersion: gv, APIResources: rs})
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c, err := NewCollector(ctx, dyn, kube)
	if err != nil {
		t.Fatalf("NewCollector: %v", err)
	}
	return c
}

// eventually polls until the collector output matches expected for names.
func eventually(t *testing.T, c prometheus.Collector, expected string, names ...string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var err error
	for {
		err = testutil.CollectAndCompare(c, strings.NewReader(expected), names...)
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("collector output did not match: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

var (
	nadGVK  = schema.GroupVersionKind{Group: "k8s.cni.cncf.io", Version: "v1", Kind: "NetworkAttachmentDefinition"}
	mnpGVK  = schema.GroupVersionKind{Group: "k8s.cni.cncf.io", Version: "v1beta1", Kind: "MultiNetworkPolicy"}
	udnGVK  = schema.GroupVersionKind{Group: "k8s.ovn.org", Version: "v1", Kind: "UserDefinedNetwork"}
	cudnGVK = schema.GroupVersionKind{Group: "k8s.ovn.org", Version: "v1", Kind: "ClusterUserDefinedNetwork"}
)

func TestCollectorCountsNADsByOwner(t *testing.T) {
	udnOwned := obj(nadGVK, "tenant-a", "n3", nil)
	udnOwned.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "k8s.ovn.org/v1", Kind: "UserDefinedNetwork", Name: "x", UID: "u"}})
	other := obj(nadGVK, "default", "n2", nil)
	other.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "d", UID: "d"}})
	c := newCollector(t, allGVRs, nil, obj(nadGVK, "default", "n1", nil), other, udnOwned)
	eventually(t, c, `
# HELP ovnkube_clustermanager_network_attachment_definitions Number of NetworkAttachmentDefinitions.
# TYPE ovnkube_clustermanager_network_attachment_definitions gauge
ovnkube_clustermanager_network_attachment_definitions{managed_by="udn",namespace="tenant-a"} 1
ovnkube_clustermanager_network_attachment_definitions{managed_by="user",namespace="default"} 2
`, "ovnkube_clustermanager_network_attachment_definitions")
}

func TestCollectorCountsMNPTargets(t *testing.T) {
	a := obj(mnpGVK, "repro", "a", nil)
	a.SetAnnotations(map[string]string{"k8s.v1.cni.cncf.io/policy-for": "default/a,default/b,default/c"})
	b := obj(mnpGVK, "repro", "b", nil)
	b.SetAnnotations(map[string]string{"k8s.v1.cni.cncf.io/policy-for": "default/a"})
	c := newCollector(t, allGVRs, nil, a, b)
	eventually(t, c, `
# HELP ovnkube_clustermanager_multi_network_policies Number of MultiNetworkPolicies.
# TYPE ovnkube_clustermanager_multi_network_policies gauge
ovnkube_clustermanager_multi_network_policies{namespace="repro"} 2
# HELP ovnkube_clustermanager_multi_network_policy_network_targets Number of networks targeted by MultiNetworkPolicies (policy-for entries).
# TYPE ovnkube_clustermanager_multi_network_policy_network_targets gauge
ovnkube_clustermanager_multi_network_policy_network_targets{namespace="repro"} 4
`, "ovnkube_clustermanager_multi_network_policies", "ovnkube_clustermanager_multi_network_policy_network_targets")
}

func TestCountPolicyTargets(t *testing.T) {
	for in, want := range map[string]int{"": 0, "a": 1, "a,b": 2, " a , ,b,": 2, ",,": 0} {
		if got := countPolicyTargets(in); got != want {
			t.Errorf("countPolicyTargets(%q)=%d want %d", in, got, want)
		}
	}
}

func TestCollectorCountsUDNs(t *testing.T) {
	udn := obj(udnGVK, "tenant-a", "u1", map[string]interface{}{"spec": map[string]interface{}{
		"topology": "Layer2", "layer2": map[string]interface{}{"role": "Primary"}}})
	cudn := obj(cudnGVK, "", "c1", map[string]interface{}{"spec": map[string]interface{}{
		"network": map[string]interface{}{"topology": "Layer3", "layer3": map[string]interface{}{"role": "Secondary"}}}})
	c := newCollector(t, allGVRs, nil, udn, cudn)
	eventually(t, c, `
# HELP ovnkube_clustermanager_user_defined_networks Number of UserDefinedNetworks and ClusterUserDefinedNetworks.
# TYPE ovnkube_clustermanager_user_defined_networks gauge
ovnkube_clustermanager_user_defined_networks{kind="UDN",role="Primary",topology="Layer2"} 1
ovnkube_clustermanager_user_defined_networks{kind="CUDN",role="Secondary",topology="Layer3"} 1
`, "ovnkube_clustermanager_user_defined_networks")
}

func TestCollectorCountsNetworkPolicies(t *testing.T) {
	mk := func(ns, n string) *networkingv1.NetworkPolicy {
		return &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: n}}
	}
	c := newCollector(t, allGVRs, []*networkingv1.NetworkPolicy{mk("a", "1"), mk("a", "2"), mk("b", "1")})
	eventually(t, c, `
# HELP ovnkube_clustermanager_network_policies Number of NetworkPolicies.
# TYPE ovnkube_clustermanager_network_policies gauge
ovnkube_clustermanager_network_policies{namespace="a"} 2
ovnkube_clustermanager_network_policies{namespace="b"} 1
`, "ovnkube_clustermanager_network_policies")
}

func TestCollectorMissingCRDNotFatal(t *testing.T) {
	// UDN and CUDN CRDs absent.
	c := newCollector(t, []schema.GroupVersionResource{nadGVR, mnpGVR}, nil, obj(nadGVK, "default", "n1", nil))
	eventually(t, c, `
# HELP ovnk_observ_informer_synced Whether the informer for a resource has completed its initial sync.
# TYPE ovnk_observ_informer_synced gauge
ovnk_observ_informer_synced{resource="cudn"} 0
ovnk_observ_informer_synced{resource="mnp"} 1
ovnk_observ_informer_synced{resource="nad"} 1
ovnk_observ_informer_synced{resource="networkpolicy"} 1
ovnk_observ_informer_synced{resource="udn"} 0
`, "ovnk_observ_informer_synced")
	if n := testutil.CollectAndCount(c, "ovnkube_clustermanager_user_defined_networks"); n != 0 {
		t.Fatalf("expected no UDN series, got %d", n)
	}
}

func TestCollectorConcurrentScrapes(t *testing.T) {
	c := newCollector(t, allGVRs, nil, obj(nadGVK, "default", "n1", nil))
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 50; j++ {
				testutil.CollectAndCount(c)
			}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}
