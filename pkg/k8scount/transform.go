package k8scount

import (
	"strconv"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Informer transforms. Each replaces a cached object with a fresh minimal
// copy holding only what Collect reads, so the informer caches do not retain
// specs, managedFields, status or unrelated annotations (on the lab cluster
// 3150 MNPs are 54 MB of JSON). They are idempotent and pass through
// anything that is not the expected type (e.g. DeletedFinalStateUnknown).

// skeleton copies apiVersion, kind, name, namespace and resourceVersion and
// sets the private manager annotation.
func skeleton(u *unstructured.Unstructured) (*unstructured.Unstructured, map[string]interface{}) {
	md := map[string]interface{}{"name": u.GetName(), "namespace": u.GetNamespace(),
		"annotations": map[string]interface{}{
			managerAnnotation: latestManager(u.GetManagedFields(), u.GetAnnotations()[managerAnnotation]),
		}}
	if rv := u.GetResourceVersion(); rv != "" {
		md["resourceVersion"] = rv
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": u.GetAPIVersion(), "kind": u.GetKind(), "metadata": md,
	}}, md
}

// Private annotations the transforms attach to cached objects. They carry
// values computed from fields the transform strips; they never exist on the
// API server.
const (
	internalPrefix = "ovnk-observ.internal/"

	ingressRulesAnnotation = internalPrefix + "ingress-rules"
	egressRulesAnnotation  = internalPrefix + "egress-rules"
	ingressPeersAnnotation = internalPrefix + "ingress-peers"
	egressPeersAnnotation  = internalPrefix + "egress-peers"
	managerAnnotation      = internalPrefix + "manager"
)

// latestManager returns the Manager of the managedFields entry with the
// latest Time; on equal or nil times the last such entry wins. With no
// entries it keeps prev (an already-transformed object) or "unknown".
func latestManager(entries []metav1.ManagedFieldsEntry, prev string) string {
	if len(entries) == 0 {
		if prev != "" {
			return prev
		}
		return unknownManager
	}
	best, bestT := "", time.Time{}
	for _, e := range entries {
		var t time.Time
		if e.Time != nil {
			t = e.Time.Time
		}
		if !t.Before(bestT) {
			best, bestT = e.Manager, t
		}
	}
	return best
}

// ruleCounts returns the number of rules in spec[ruleKey] and the total
// number of entries in each rule's peerKey list. Malformed rules count as
// rules with no peers; nothing here panics on unexpected shapes.
func ruleCounts(u *unstructured.Unstructured, ruleKey, peerKey string) (rules, peers int) {
	// NoCopy: NestedSlice deep-copies and panics on non-JSON values.
	raw, _, _ := unstructured.NestedFieldNoCopy(u.Object, "spec", ruleKey)
	list, _ := raw.([]interface{})
	for _, r := range list {
		rules++
		if rm, ok := r.(map[string]interface{}); ok {
			if ps, ok := rm[peerKey].([]interface{}); ok {
				peers += len(ps)
			}
		}
	}
	return rules, peers
}

// transformMNP keeps metadata, the policy-for annotation and the rule and
// peer counts of spec.ingress / spec.egress.
func transformMNP(obj interface{}) (interface{}, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return obj, nil
	}
	out, md := skeleton(u)
	ann := md["annotations"].(map[string]interface{})
	if v, found := u.GetAnnotations()[policyForAnnotation]; found {
		ann[policyForAnnotation] = v
	}
	set := func(key string, n int) { ann[key] = strconv.Itoa(n) }
	if _, found, _ := unstructured.NestedFieldNoCopy(u.Object, "spec"); found {
		ir, ip := ruleCounts(u, "ingress", "from")
		er, ep := ruleCounts(u, "egress", "to")
		set(ingressRulesAnnotation, ir)
		set(ingressPeersAnnotation, ip)
		set(egressRulesAnnotation, er)
		set(egressPeersAnnotation, ep)
	} else {
		// Already transformed (idempotence): keep the counts it carries.
		for _, k := range []string{ingressRulesAnnotation, egressRulesAnnotation, ingressPeersAnnotation, egressPeersAnnotation} {
			if v, found := u.GetAnnotations()[k]; found {
				ann[k] = v
			}
		}
	}
	return out, nil
}

// transformNAD keeps metadata plus ownerReferences (for managed_by).
func transformNAD(obj interface{}) (interface{}, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return obj, nil
	}
	out, md := skeleton(u)
	if refs, found, _ := unstructured.NestedSlice(u.Object, "metadata", "ownerReferences"); found && len(refs) > 0 {
		md["ownerReferences"] = refs
	}
	return out, nil
}

// networkShape copies topology and layer2/layer3 role from src.
func networkShape(src map[string]interface{}) map[string]interface{} {
	dst := map[string]interface{}{}
	if t, ok := src["topology"].(string); ok {
		dst["topology"] = t
	}
	for _, l := range []string{"layer2", "layer3"} {
		if lm, ok := src[l].(map[string]interface{}); ok {
			if r, ok := lm["role"].(string); ok {
				dst[l] = map[string]interface{}{"role": r}
			}
		}
	}
	return dst
}

func transformUDNAt(obj interface{}, path ...string) (interface{}, error) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return obj, nil
	}
	out, _ := skeleton(u)
	src, found, _ := unstructured.NestedMap(u.Object, path...)
	if !found {
		return out, nil
	}
	shape := networkShape(src)
	if len(shape) == 0 {
		return out, nil
	}
	_ = unstructured.SetNestedMap(out.Object, shape, path...)
	return out, nil
}

// transformUDN keeps metadata plus spec.topology and spec.layer{2,3}.role.
func transformUDN(obj interface{}) (interface{}, error) { return transformUDNAt(obj, "spec") }

// transformCUDN keeps metadata plus the same fields under spec.network.
func transformCUDN(obj interface{}) (interface{}, error) {
	return transformUDNAt(obj, "spec", "network")
}

// transformNetworkPolicy keeps name, namespace, resourceVersion and the
// manager annotation only.
func transformNetworkPolicy(obj interface{}) (interface{}, error) {
	p, ok := obj.(*networkingv1.NetworkPolicy)
	if !ok {
		return obj, nil
	}
	return &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{
		Name: p.Name, Namespace: p.Namespace, ResourceVersion: p.ResourceVersion,
		Annotations: map[string]string{
			managerAnnotation: latestManager(p.ManagedFields, p.Annotations[managerAnnotation]),
		},
	}}, nil
}
