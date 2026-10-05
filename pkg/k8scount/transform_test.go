package k8scount

import (
	"reflect"
	"strings"
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"
)

// fat returns an object carrying everything a real API object might: labels,
// extra annotations, managedFields, a large spec and a status.
func fat(gvk schema.GroupVersionKind, spec map[string]interface{}) *unstructured.Unstructured {
	u := obj(gvk, "ns1", "o1", map[string]interface{}{
		"spec":   spec,
		"status": map[string]interface{}{"conditions": []interface{}{"x"}},
	})
	u.SetResourceVersion("42")
	u.SetUID("uid-1")
	u.SetLabels(map[string]string{"l": "v"})
	u.SetAnnotations(map[string]string{
		policyForAnnotation: "default/a,default/b",
		"kubectl.kubernetes.io/last-applied-configuration": "{huge}",
	})
	u.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: "kubectl", Operation: metav1.ManagedFieldsOperationApply}})
	u.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "k8s.ovn.org/v1", Kind: "UserDefinedNetwork", Name: "x", UID: "u"}})
	return u
}

// minimal builds the expected transform output.
func minimal(gvk schema.GroupVersionKind, metadata map[string]interface{}, rest map[string]interface{}) *unstructured.Unstructured {
	md := map[string]interface{}{"name": "o1", "namespace": "ns1", "resourceVersion": "42"}
	for k, v := range metadata {
		md[k] = v
	}
	o := map[string]interface{}{"apiVersion": gvk.GroupVersion().String(), "kind": gvk.Kind, "metadata": md}
	for k, v := range rest {
		o[k] = v
	}
	return &unstructured.Unstructured{Object: o}
}

func TestTransformsKeepOnlyCollectorFields(t *testing.T) {
	udnSpec := map[string]interface{}{
		"topology": "Layer3",
		"layer3":   map[string]interface{}{"role": "Primary", "subnets": []interface{}{"10.0.0.0/16"}},
		"layer2":   map[string]interface{}{"role": "Secondary", "subnets": []interface{}{"10.1.0.0/16"}},
		"other":    "drop",
	}
	cases := []struct {
		name string
		fn   cache.TransformFunc
		in   *unstructured.Unstructured
		want *unstructured.Unstructured
	}{
		{"mnp", transformMNP,
			fat(mnpGVK, map[string]interface{}{"podSelector": map[string]interface{}{}, "ingress": []interface{}{"big"}}),
			minimal(mnpGVK, map[string]interface{}{"annotations": map[string]interface{}{
				policyForAnnotation:    "default/a,default/b",
				ingressRulesAnnotation: "1", ingressPeersAnnotation: "0",
				egressRulesAnnotation: "0", egressPeersAnnotation: "0"}}, nil)},
		{"nad", transformNAD,
			fat(nadGVK, map[string]interface{}{"config": `{"cniVersion":"0.4.0"}`}),
			minimal(nadGVK, map[string]interface{}{"ownerReferences": []interface{}{map[string]interface{}{
				"apiVersion": "k8s.ovn.org/v1", "kind": "UserDefinedNetwork", "name": "x", "uid": "u"}}}, nil)},
		{"udn", transformUDN,
			fat(udnGVK, udnSpec),
			minimal(udnGVK, nil, map[string]interface{}{"spec": map[string]interface{}{
				"topology": "Layer3",
				"layer3":   map[string]interface{}{"role": "Primary"},
				"layer2":   map[string]interface{}{"role": "Secondary"},
			}})},
		{"cudn", transformCUDN,
			fat(cudnGVK, map[string]interface{}{"namespaceSelector": map[string]interface{}{}, "network": map[string]interface{}{
				"topology": "Layer2", "layer2": map[string]interface{}{"role": "Secondary", "subnets": []interface{}{"x"}}}}),
			minimal(cudnGVK, nil, map[string]interface{}{"spec": map[string]interface{}{"network": map[string]interface{}{
				"topology": "Layer2", "layer2": map[string]interface{}{"role": "Secondary"}}}})},
		{"mnp without annotation", transformMNP,
			obj(mnpGVK, "ns1", "o1", map[string]interface{}{"spec": map[string]interface{}{"x": "y"}}),
			&unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": mnpGVK.GroupVersion().String(), "kind": mnpGVK.Kind,
				"metadata": map[string]interface{}{"name": "o1", "namespace": "ns1", "annotations": map[string]interface{}{
					ingressRulesAnnotation: "0", ingressPeersAnnotation: "0",
					egressRulesAnnotation: "0", egressPeersAnnotation: "0"}}}}},
		{"udn without spec", transformUDN,
			obj(udnGVK, "ns1", "o1", nil),
			&unstructured.Unstructured{Object: map[string]interface{}{"apiVersion": udnGVK.GroupVersion().String(), "kind": udnGVK.Kind,
				"metadata": map[string]interface{}{"name": "o1", "namespace": "ns1"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.fn(tc.in)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got  %#v\nwant %#v", got.(*unstructured.Unstructured).Object, tc.want.Object)
			}
			// Idempotent: the informer may transform an already-transformed object.
			again, err := tc.fn(got)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(again, tc.want) {
				t.Fatalf("not idempotent: %#v", again.(*unstructured.Unstructured).Object)
			}
		})
	}
}

func TestTransformNetworkPolicyKeepsNameNamespace(t *testing.T) {
	in := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", ResourceVersion: "7", UID: "u",
			Labels: map[string]string{"a": "b"}, Annotations: map[string]string{"c": "d"},
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "m"}}},
		Spec: networkingv1.NetworkPolicySpec{PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}},
	}
	got, err := transformNetworkPolicy(in)
	if err != nil {
		t.Fatal(err)
	}
	want := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", ResourceVersion: "7"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
}

func TestTransformsPassThroughOtherTypes(t *testing.T) {
	tomb := cache.DeletedFinalStateUnknown{Key: "ns/x", Obj: nil}
	for name, fn := range map[string]cache.TransformFunc{
		"mnp": transformMNP, "nad": transformNAD, "udn": transformUDN, "cudn": transformCUDN, "np": transformNetworkPolicy,
	} {
		got, err := fn(tomb)
		if err != nil || !reflect.DeepEqual(got, tomb) {
			t.Fatalf("%s: got %#v, %v", name, got, err)
		}
	}
}

// TestCollectorCachesTransformedObjects proves NewCollector registers the
// transforms: cached objects lack spec, managedFields and other annotations.
func TestCollectorCachesTransformedObjects(t *testing.T) {
	m := fat(mnpGVK, map[string]interface{}{"ingress": []interface{}{"big"}})
	m.SetManagedFields(nil) // the fake tracker rejects apply managedFields on create
	np := &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns",
		Annotations: map[string]string{"c": "d"}},
		Spec: networkingv1.NetworkPolicySpec{PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}}}
	c := newCollector(t, allGVRs, []*networkingv1.NetworkPolicy{np}, m).(*collector)
	deadline := time.Now().Add(2 * time.Second)
	for {
		mnps, ok1 := c.list("mnp")
		nps, ok2 := c.list("networkpolicy")
		if ok1 && ok2 && len(mnps) == 1 && len(nps) == 1 {
			u := mnps[0].(*unstructured.Unstructured)
			if _, has := u.Object["spec"]; has {
				t.Fatalf("mnp spec cached: %#v", u.Object)
			}
			if _, has := u.Object["status"]; has {
				t.Fatalf("mnp status cached: %#v", u.Object)
			}
			a := u.GetAnnotations()
			if a[policyForAnnotation] == "" {
				t.Fatalf("mnp annotations: %v", a)
			}
			for k := range a {
				if k != policyForAnnotation && !strings.HasPrefix(k, internalPrefix) {
					t.Fatalf("mnp unexpected annotation %q: %v", k, a)
				}
			}
			if len(u.GetLabels()) != 0 {
				t.Fatalf("mnp labels cached: %v", u.GetLabels())
			}
			p := nps[0].(*networkingv1.NetworkPolicy)
			if len(p.Spec.PolicyTypes) != 0 || len(p.Annotations) != 0 {
				t.Fatalf("networkpolicy not stripped: %#v", p)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("informers did not sync")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTransformMNPKeepsCounts(t *testing.T) {
	spec := map[string]interface{}{
		"ingress": []interface{}{
			map[string]interface{}{"from": []interface{}{"a", "b", "c"}},
			map[string]interface{}{},
		},
		"egress": []interface{}{map[string]interface{}{"to": []interface{}{int64(1), int64(2), int64(3), int64(4)}}},
	}
	got, err := transformMNP(fat(mnpGVK, spec))
	if err != nil {
		t.Fatal(err)
	}
	u := got.(*unstructured.Unstructured)
	want := map[string]string{ingressRulesAnnotation: "2", egressRulesAnnotation: "1", ingressPeersAnnotation: "3", egressPeersAnnotation: "4"}
	for k, v := range want {
		if u.GetAnnotations()[k] != v {
			t.Fatalf("%s = %q, want %q", k, u.GetAnnotations()[k], v)
		}
	}
	if _, has := u.Object["spec"]; has {
		t.Fatal("spec cached")
	}
	if len(u.GetManagedFields()) != 0 {
		t.Fatal("managedFields cached")
	}
	// Idempotent: counts survive a second transform.
	again, _ := transformMNP(got)
	if !reflect.DeepEqual(again, got) {
		t.Fatalf("not idempotent: %#v", again)
	}
}

func TestTransformMNPMalformedSpecDoesNotPanic(t *testing.T) {
	for _, spec := range []interface{}{
		"str", []interface{}{},
		map[string]interface{}{"ingress": "x", "egress": 5},
		map[string]interface{}{"ingress": []interface{}{"s", nil, map[string]interface{}{"from": "bad"}}},
	} {
		u := obj(mnpGVK, "ns1", "o1", map[string]interface{}{"spec": spec})
		if _, err := transformMNP(u); err != nil {
			t.Fatal(err)
		}
	}
}
