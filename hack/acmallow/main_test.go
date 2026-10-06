package main

import (
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const ours = `apiVersion: v1
kind: ConfigMap
metadata:
  name: observability-metrics-custom-allowlist
  namespace: open-cluster-management-observability
data:
  metrics_list.yaml: |
    names:
      - ovnk:pg_drift:nodes
      - ovn_northd_inc_engine_runs_total
`

const live = `apiVersion: v1
kind: ConfigMap
metadata:
  name: observability-metrics-custom-allowlist
  namespace: open-cluster-management-observability
  resourceVersion: "12345"
  uid: 0d6c0a51-0000-0000-0000-000000000000
  creationTimestamp: "2026-02-10T00:00:00Z"
  labels:
    team: platform
  annotations:
    kubectl.kubernetes.io/last-applied-configuration: '{"big":"blob"}'
    owner: netops
  managedFields:
  - manager: kubectl
data:
  uwl_metrics_list.yaml: |
    names:
      - my_app_requests_total
  metrics_list.yaml: |
    names:
      - etcd_server_is_leader
      - ovn_northd_inc_engine_runs_total
    matches:
      - __name__="up",job="x"
    recording_rules:
      - record: vm:mtv_net_throughput:max5m
        expr: label_replace(max_over_time(mtv_migration_net_throughput{plan_name=~"mtv-.+"}[5m]), "vm", "$1", "plan_name", "mtv-(.*)")
`

func mustMerge(t *testing.T, live string) (ConfigMap, []string) {
	t.Helper()
	out, added, err := Merge([]byte(live), []byte(ours))
	if err != nil {
		t.Fatal(err)
	}
	var cm ConfigMap
	if err := yaml.Unmarshal(out, &cm); err != nil {
		t.Fatalf("output is not YAML: %v\n%s", err, out)
	}
	return cm, added
}

func list(t *testing.T, cm ConfigMap) map[string]any {
	t.Helper()
	var l map[string]any
	if err := yaml.Unmarshal([]byte(cm.Data[listKey]), &l); err != nil {
		t.Fatal(err)
	}
	return l
}

func TestMergeAddsOnlyMissingNames(t *testing.T) {
	cm, added := mustMerge(t, live)
	if want := []string{"ovnk:pg_drift:nodes"}; !reflect.DeepEqual(added, want) {
		t.Errorf("added = %q, want %q", added, want)
	}
	got := list(t, cm)["names"]
	want := []any{"etcd_server_is_leader", "ovn_northd_inc_engine_runs_total", "ovnk:pg_drift:nodes"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("names = %v, want %v (existing order kept, new names appended)", got, want)
	}
}

func TestMergeKeepsEverythingElse(t *testing.T) {
	cm, _ := mustMerge(t, live)
	l := list(t, cm)
	rules := l["recording_rules"].([]any)
	r := rules[0].(map[string]any)
	if r["record"] != "vm:mtv_net_throughput:max5m" ||
		r["expr"] != `label_replace(max_over_time(mtv_migration_net_throughput{plan_name=~"mtv-.+"}[5m]), "vm", "$1", "plan_name", "mtv-(.*)")` {
		t.Errorf("recording rule changed: %v", r)
	}
	if !reflect.DeepEqual(l["matches"], []any{`__name__="up",job="x"`}) {
		t.Errorf("matches changed: %v", l["matches"])
	}
	if !strings.Contains(cm.Data["uwl_metrics_list.yaml"], "my_app_requests_total") {
		t.Error("other data keys must be kept as-is")
	}
	if cm.Metadata.Labels["team"] != "platform" || cm.Metadata.Annotations["owner"] != "netops" {
		t.Errorf("labels/annotations not kept: %+v", cm.Metadata)
	}
}

func TestMergeStripsServerFields(t *testing.T) {
	out, _, err := Merge([]byte(live), []byte(ours))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"uid:", "creationTimestamp:", "managedFields:", "last-applied-configuration"} {
		if strings.Contains(string(out), f) {
			t.Errorf("output still contains %s", f)
		}
	}
	// resourceVersion stays so a concurrent edit makes the apply fail instead of being lost.
	if !strings.Contains(string(out), `resourceVersion: "12345"`) {
		t.Error("resourceVersion must be kept")
	}
}

func TestMergeIdempotent(t *testing.T) {
	once, _, err := Merge([]byte(live), []byte(ours))
	if err != nil {
		t.Fatal(err)
	}
	twice, added, err := Merge(once, []byte(ours))
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 0 || string(once) != string(twice) {
		t.Errorf("second merge added %q or changed output", added)
	}
}

func TestMergeNoLiveConfigMap(t *testing.T) {
	cm, added := mustMerge(t, "")
	if len(added) != 2 {
		t.Errorf("added = %q, want both names", added)
	}
	if cm.Metadata.Name != cmName || cm.Metadata.Namespace != cmNS {
		t.Errorf("metadata = %+v", cm.Metadata)
	}
}

func TestMergeRejectsWrongObject(t *testing.T) {
	bad := strings.Replace(live, "name: observability-metrics-custom-allowlist", "name: something-else", 1)
	if _, _, err := Merge([]byte(bad), []byte(ours)); err == nil {
		t.Error("want an error for a ConfigMap with another name")
	}
}

// Only the appended name lines may differ from the live text, so `oc diff`
// shows just the additions.
func TestMergeLeavesLiveTextIntact(t *testing.T) {
	out, _, err := Merge([]byte(live), []byte(ours))
	if err != nil {
		t.Fatal(err)
	}
	var in, got ConfigMap
	if err := yaml.Unmarshal([]byte(live), &in); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(in.Data[listKey],
		"  - ovn_northd_inc_engine_runs_total\n",
		"  - ovn_northd_inc_engine_runs_total\n  - ovnk:pg_drift:nodes\n", 1)
	if want == in.Data[listKey] {
		t.Fatal("test fixture did not change; fix the expected text")
	}
	if got.Data[listKey] != want {
		t.Errorf("metrics_list.yaml =\n%s\nwant\n%s", got.Data[listKey], want)
	}
}

func TestMergeAddsNamesKeyWhenMissing(t *testing.T) {
	noNames := strings.Replace(live, "    names:\n      - etcd_server_is_leader\n      - ovn_northd_inc_engine_runs_total\n", "", 1)
	cm, added := mustMerge(t, noNames)
	if len(added) != 2 {
		t.Errorf("added = %q", added)
	}
	l := list(t, cm)
	if !reflect.DeepEqual(l["names"], []any{"ovnk:pg_drift:nodes", "ovn_northd_inc_engine_runs_total"}) || l["recording_rules"] == nil {
		t.Errorf("list = %v", l)
	}
}

func TestMergeRejectsFlowStyleNames(t *testing.T) {
	flow := strings.Replace(live, "    names:\n      - etcd_server_is_leader\n      - ovn_northd_inc_engine_runs_total\n", "    names: [etcd_server_is_leader]\n", 1)
	if _, _, err := Merge([]byte(flow), []byte(ours)); err == nil {
		t.Error("want an error asking for a manual merge of a flow-style names list")
	}
}
