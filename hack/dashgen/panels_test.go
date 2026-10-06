package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

const (
	wantTitle     = "Networking / OVN-K Observ"
	wantCMName    = "grafana-dashboard-ovn-scale-troubleshooting"
	wantCMNS      = "openshift-config-managed"
	wantCMLabel   = `console.openshift.io/dashboard: "true"`
	wantCMDataKey = "ovn-scale-troubleshooting.json"
)

func TestDashboardRows(t *testing.T) {
	d := Build(Options{})
	want := []string{
		"At a glance",
		"Consistency",
		"Change & churn",
		"Scale & fan-out",
		"Programming latency",
		"OVN internals",
		"Exporter self-cost",
	}
	var got []string
	for _, r := range d.Rows {
		got = append(got, r.Title)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("row titles = %q, want %q", got, want)
	}
	// Only the last two rows (OVN internals, Exporter self-cost) start collapsed.
	for i, r := range d.Rows {
		want := i >= len(d.Rows)-2
		if r.Collapse != want {
			t.Errorf("row %q collapse = %v, want %v", r.Title, r.Collapse, want)
		}
	}
}

func TestPanelsValid(t *testing.T) {
	// "gauge" renders as a plain singlestat in the console, so it is not used.
	allowed := map[string]bool{"graph": true, "singlestat": true, "table": true, "row": true}
	seen := map[int]string{}
	d := Build(Options{})
	if len(d.Rows) == 0 {
		t.Fatal("dashboard has no rows")
	}
	for _, r := range d.Rows {
		if len(r.Panels) == 0 {
			t.Errorf("row %q has no panels", r.Title)
		}
		span := 0
		for _, p := range r.Panels {
			span += p.Span
			if prev, ok := seen[p.ID]; ok {
				t.Errorf("panel %q reuses id %d of %q", p.Title, p.ID, prev)
			}
			seen[p.ID] = p.Title
			if p.ID <= 0 {
				t.Errorf("panel %q has non-positive id %d", p.Title, p.ID)
			}
			if !allowed[p.Type] {
				t.Errorf("panel %q has type %q not in allowed set", p.Title, p.Type)
			}
			if p.Span <= 0 || p.Span > 12 {
				t.Errorf("panel %q has span %d", p.Title, p.Span)
			}
			if len(p.Targets) == 0 {
				t.Errorf("panel %q has no targets", p.Title)
			}
			for i, tg := range p.Targets {
				e := strings.TrimSpace(tg.Expr)
				if e == "" {
					t.Errorf("panel %q target %d has empty expr", p.Title, i)
				}
				if strings.Contains(e, "NODE_JOIN") || strings.Contains(e, " J)") || strings.HasSuffix(e, " J") {
					t.Errorf("panel %q target %d has join placeholder: %s", p.Title, i, e)
				}
				if strings.Count(e, "(") != strings.Count(e, ")") {
					t.Errorf("panel %q target %d has unbalanced parens: %s", p.Title, i, e)
				}
				if p.Type == "table" && !(tg.Instant && tg.Format == "table") {
					t.Errorf("table panel %q target %d must be instant+format table", p.Title, i)
				}
			}
		}
		if span%12 != 0 {
			t.Errorf("row %q spans %d columns, not whole 12-column lines", r.Title, span)
		}
	}
}

func TestJoinHelper(t *testing.T) {
	got := join("ovn_db_db_size_bytes")
	want := `ovn_db_db_size_bytes * on (namespace, pod) group_left(node) ovnk:ovn_pod_node:info{node=~"$node"}`
	if got != want {
		t.Fatalf("join = %s\nwant  %s", got, want)
	}
}

func TestTitleAndVariables(t *testing.T) {
	d := Build(Options{})
	if d.Title != wantTitle {
		t.Errorf("title = %q, want %q", d.Title, wantTitle)
	}
	var names []string
	for _, v := range d.Templating.List {
		names = append(names, v.Name)
	}
	if want := []string{"datasource", "node", "network", "interval"}; !reflect.DeepEqual(names, want) {
		t.Errorf("variables = %q, want %q", names, want)
	}
	byName := map[string]Variable{}
	for _, v := range d.Templating.List {
		byName[v.Name] = v
	}
	if v := byName["datasource"]; v.Type != "datasource" || v.Query != "prometheus" {
		t.Errorf("datasource var = %+v", v)
	}
	for _, n := range []string{"node", "network"} {
		v := byName[n]
		if v.Type != "query" || !v.IncludeAll || v.AllValue != ".*" ||
			v.Query != "label_values(ovnkube_controller_nb_db_objects, "+n+")" {
			t.Errorf("%s var = %+v", n, v)
		}
	}
	if v := byName["interval"]; v.Type != "interval" || v.Query != "1m,5m,15m" || v.Current.Value != "5m" {
		t.Errorf("interval var = %+v", v)
	}
	if len(d.Links) != 1 || d.Links[0].Title != "Networking / Infrastructure" {
		t.Errorf("links = %+v", d.Links)
	}
}

func TestConfigMapWraps(t *testing.T) {
	d := Build(Options{})
	y, err := ConfigMapYAML(d)
	if err != nil {
		t.Fatal(err)
	}
	s := string(y)
	for _, want := range []string{
		"apiVersion: v1\n",
		"kind: ConfigMap\n",
		"  name: " + wantCMName + "\n",
		"  namespace: " + wantCMNS + "\n",
		"  labels:\n    " + wantCMLabel + "\n",
		"data:\n  " + wantCMDataKey + ": |-\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("configmap missing %q", want)
		}
	}

	// Pull the block scalar back out and compare it with Build(Options{}).
	var block bytes.Buffer
	in := false
	sc := bufio.NewScanner(bytes.NewReader(y))
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		line := sc.Text()
		if line == "  "+wantCMDataKey+": |-" {
			in = true
			continue
		}
		if in {
			if !strings.HasPrefix(line, "    ") && line != "" {
				break
			}
			block.WriteString(strings.TrimPrefix(line, "    "))
			block.WriteByte('\n')
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	var fromCM, fromBuild any
	if err := json.Unmarshal(block.Bytes(), &fromCM); err != nil {
		t.Fatalf("configmap data is not JSON: %v", err)
	}
	b, err := DashboardJSON(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &fromBuild); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromCM, fromBuild) {
		t.Fatal("configmap JSON differs from Build(Options{}) output")
	}
	if strings.Contains(string(b), "\\u0026") {
		t.Error("dashboard JSON HTML-escapes '&'; disable HTML escaping")
	}
}

func panelByTitle(t *testing.T, d Dashboard, title string) Panel {
	t.Helper()
	for _, r := range d.Rows {
		for _, p := range r.Panels {
			if p.Title == title {
				return p
			}
		}
	}
	t.Fatalf("no panel titled %q", title)
	return Panel{}
}

// consoleColor mirrors the console SingleStat lookup:
// sortedIndexBy(steps, Number(value)) - 1, with a null step value read as 0.
func consoleColor(steps []Threshold, v float64) string {
	i := 0
	for _, s := range steps {
		sv := 0.0
		if s.Value != nil {
			sv = *s.Value
		}
		if sv < v {
			i++
		}
	}
	if i == 0 {
		return ""
	}
	return steps[i-1].Color
}

func TestPanelCount(t *testing.T) {
	n := 0
	for _, r := range Build(Options{}).Rows {
		n += len(r.Panels)
	}
	if n != 52 {
		t.Errorf("panel count = %d, want 52", n)
	}
}

func TestSinglestatColorsUseFieldOptions(t *testing.T) {
	for _, r := range Build(Options{}).Rows {
		for _, p := range r.Panels {
			if p.Type != "singlestat" {
				continue
			}
			b, err := json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatal(err)
			}
			for _, k := range []string{"thresholds", "colors", "colorValue", "gauge"} {
				if _, ok := m[k]; ok {
					t.Errorf("singlestat %q sets %q, which the console ignores", p.Title, k)
				}
			}
		}
	}
	d := Build(Options{})
	cases := []struct {
		title string
		want  map[float64]string
	}{
		{"PortGroup amplification", map[float64]string{0: "green", 4.9: "green", 5: "light-yellow", 9.99: "light-yellow", 10: "light-red", 143.6: "light-red"}},
		{"Nodes disconnected", map[float64]string{0: "green", 1: "light-red", 3: "light-red"}},
		{"Nodes with PG drift", map[float64]string{0: "green", 1: "light-red", 4: "light-red"}},
		{"Worst northd recompute ratio", map[float64]string{0: "green", 0.19: "green", 0.2: "light-yellow", 0.49: "light-yellow", 0.5: "light-red", 1: "light-red"}},
		{"Max NB→SB lag", map[float64]string{0: "green", 9.9: "green", 10: "light-yellow", 29: "light-yellow", 30: "light-red"}},
		{"ovnkube restarts 1h", map[float64]string{0: "green", 1: "light-yellow", 4: "light-yellow", 5: "light-red"}},
		{"Nodes not Ready", map[float64]string{0: "green", 1: "light-red", 2: "light-red"}},
	}
	for _, c := range cases {
		p := panelByTitle(t, d, c.title)
		if p.Type != "singlestat" {
			t.Errorf("%q type = %q, want singlestat", c.title, p.Type)
		}
		if p.Options == nil {
			t.Errorf("%q has no options.fieldOptions.thresholds", c.title)
			continue
		}
		for v, want := range c.want {
			if got := consoleColor(p.Options.FieldOptions.Thresholds, v); got != want {
				t.Errorf("%q value %g colors %q, want %q", c.title, v, got, want)
			}
		}
	}
	if p := panelByTitle(t, d, "Nodes disconnected"); !strings.Contains(p.Targets[0].Expr, "count(count by (namespace, pod) (") {
		t.Errorf("Nodes disconnected counts series, not pods/nodes: %s", p.Targets[0].Expr)
	}
	if p := panelByTitle(t, d, "PortGroup amplification"); p.Description != "≥5 warn, ≥10 critical (alert fires >10)" {
		t.Errorf("amplification description = %q", p.Description)
	}
}

func TestNetworkProgrammingPanels(t *testing.T) {
	d := Build(Options{})
	lat := panelByTitle(t, d, "Network programming p50/p99 (1h window)")
	wantLegends := []string{"{{kind}} p50", "{{kind}} p99", "p99 OVN part"}
	if len(lat.Targets) != len(wantLegends) {
		t.Fatalf("latency targets = %+v", lat.Targets)
	}
	for i, tg := range lat.Targets {
		if tg.LegendFormat != wantLegends[i] {
			t.Errorf("latency target %d legend = %q, want %q", i, tg.LegendFormat, wantLegends[i])
		}
		if !strings.Contains(tg.Expr, "[1h])") || strings.Contains(tg.Expr, "$interval") {
			t.Errorf("latency target %d not on a fixed 1h window: %s", i, tg.Expr)
		}
		if !strings.Contains(tg.Expr, `ovnk:ovn_pod_node:info{node=~"$node"}`) {
			t.Errorf("latency target %d lacks the $node join: %s", i, tg.Expr)
		}
	}
	for i := 0; i < 2; i++ {
		if !strings.Contains(lat.Targets[i].Expr, "sum by (le, kind) (") {
			t.Errorf("latency target %d not split by kind: %s", i, lat.Targets[i].Expr)
		}
	}
	if !strings.Contains(lat.Targets[2].Expr, "network_programming_ovn_duration_seconds_bucket") {
		t.Errorf("OVN part target = %s", lat.Targets[2].Expr)
	}

	ev := panelByTitle(t, d, "Network programming events/min")
	want := `sum by (kind) (` + join(`rate(ovnkube_controller_network_programming_duration_seconds_count[$interval])`) + `) * 60`
	if ev.Type != "graph" || len(ev.Targets) != 1 || ev.Targets[0].Expr != want || ev.Targets[0].LegendFormat != "{{kind}}" {
		t.Errorf("events/min panel = %+v\nwant expr %s", ev, want)
	}

	// The events graph sits right after the latency graph.
	for _, r := range d.Rows {
		for i, p := range r.Panels {
			if p.Title == lat.Title && (i+1 >= len(r.Panels) || r.Panels[i+1].Title != ev.Title) {
				t.Errorf("events/min is not next to the latency graph in row %q", r.Title)
			}
		}
	}
}

func TestPodSetupOneHour(t *testing.T) {
	p := panelByTitle(t, Build(Options{}), "Pod setup pipeline p99 (1h window)")
	if len(p.Targets) != 1 || p.Targets[0].Expr != "ovnk:pod_setup_stage:p99_1h" {
		t.Errorf("pod setup targets = %+v", p.Targets)
	}
}

// Node-mode exporter series from an old and a new pod overlap during a
// rollout; every raw use must deduplicate before summing.
func TestNodeModeSeriesDeduped(t *testing.T) {
	const d = "max without (instance, pod, endpoint, container, service) ("
	metrics := []string{"ovnkube_controller_nb_db_objects", "ovnkube_controller_sb_db_objects", "ovnkube_controller_nb_db_updates_total"}
	for _, r := range Build(Options{}).Rows {
		for _, p := range r.Panels {
			for _, tg := range p.Targets {
				for _, m := range metrics {
					n := strings.Count(tg.Expr, m)
					ok := strings.Count(tg.Expr, d+m) + strings.Count(tg.Expr, d+"rate("+m)
					if n != ok {
						t.Errorf("panel %q uses %s without dedupe: %s", p.Title, m, tg.Expr)
					}
				}
			}
		}
	}
}

func TestScaleRowLayout(t *testing.T) {
	d := Build(Options{})
	objs := panelByTitle(t, d, "Kubernetes network objects")
	for _, tg := range objs.Targets {
		if strings.Contains(tg.Expr, "multi_network_policy_network_targets") {
			t.Errorf("Kubernetes network objects still carries the MNP targets series")
		}
	}
	mnp := panelByTitle(t, d, "MultiNetworkPolicy network targets")
	if mnp.Type != "graph" || mnp.Span != 4 || mnp.YAxes[0].Format != "short" || len(mnp.Targets) != 1 ||
		!strings.Contains(mnp.Targets[0].Expr, "ovnkube_clustermanager_multi_network_policy_network_targets") {
		t.Errorf("MNP targets panel = %+v", mnp)
	}
	for title, span := range map[string]int{"Kubernetes network objects": 4, "Top networks by ACL / PortGroup": 6, "NB objects by node": 6} {
		if p := panelByTitle(t, d, title); p.Span != span {
			t.Errorf("%q span = %d, want %d", title, p.Span, span)
		}
	}
	if p := panelByTitle(t, d, "NB objects by node"); p.Type != "table" || !strings.HasPrefix(p.Targets[0].Expr, "topk(10, ") {
		t.Errorf("NB objects by node = type %q expr %q, want a topk table", p.Type, p.Targets[0].Expr)
	}
}

func TestExporterSelfCostSplit(t *testing.T) {
	d := Build(Options{})
	for title, unit := range map[string]string{"Exporter CPU": "short", "Exporter memory (working set)": "bytes"} {
		p := panelByTitle(t, d, title)
		if p.Type != "graph" || p.Span != 3 || p.YAxes[0].Format != unit || len(p.Targets) != 1 {
			t.Errorf("%q = type %q span %d unit %q targets %d", title, p.Type, p.Span, p.YAxes[0].Format, len(p.Targets))
			continue
		}
		if !strings.HasPrefix(p.Targets[0].Expr, "topk(10, sum by (node, pod) (") || p.Targets[0].LegendFormat != "{{node}} {{pod}}" {
			t.Errorf("%q target = %+v", title, p.Targets[0])
		}
	}
	if p := panelByTitle(t, d, "DB connected / initial sync"); p.Span != 6 {
		t.Errorf("exporter table span = %d, want 6", p.Span)
	}
}

func TestGraphYAxisMin(t *testing.T) {
	for _, r := range Build(Options{}).Rows {
		for _, p := range r.Panels {
			if p.Type != "graph" {
				continue
			}
			min := p.YAxes[0].Min
			if p.Title == "DB growth per hour" {
				if min != nil {
					t.Errorf("DB growth per hour pins y min %d; negative growth would be hidden", *min)
				}
			} else if min == nil || *min != 0 {
				t.Errorf("graph %q y min = %v, want 0", p.Title, min)
			}
		}
	}
}

func TestPodSetupUnstacked(t *testing.T) {
	p := panelByTitle(t, Build(Options{}), "Pod setup pipeline p99 (1h window)")
	if p.Stack {
		t.Error("per-stage p99s are not additive; panel must not stack")
	}
	if !strings.Contains(p.Description, "not additive") {
		t.Errorf("description = %q", p.Description)
	}
}

func TestRefreshTwoMinutes(t *testing.T) {
	if r := Build(Options{}).Refresh; r != "2m" {
		t.Errorf("refresh = %q, want 2m", r)
	}
}

// The node join goes through the OVN-pod recording rule; raw kube_pod_info
// matches every pod in the cluster.
func TestNoKubePodInfoInPanels(t *testing.T) {
	for _, r := range Build(Options{}).Rows {
		for _, p := range r.Panels {
			for _, tg := range p.Targets {
				if strings.Contains(tg.Expr, "kube_pod_info") {
					t.Errorf("panel %q queries kube_pod_info: %s", p.Title, tg.Expr)
				}
			}
		}
	}
}

func TestPerNodeGraphsUseTopk(t *testing.T) {
	for _, r := range Build(Options{}).Rows {
		for _, p := range r.Panels {
			if p.Type != "graph" {
				continue
			}
			for _, tg := range p.Targets {
				if strings.Contains(tg.LegendFormat, "{{node}}") && !strings.HasPrefix(tg.Expr, "topk(10, ") {
					t.Errorf("per-node graph %q target not topk(10): %s", p.Title, tg.Expr)
				}
			}
		}
	}
}

func TestACLLogLink(t *testing.T) {
	if l := Build(Options{}).Links; len(l) != 1 {
		t.Errorf("default links = %+v, want only the Networking / Infrastructure link", l)
	}
	l := Build(Options{ACLLogURL: "https://example.invalid/acl"}).Links
	if len(l) != 2 {
		t.Fatalf("links = %+v, want 2", l)
	}
	if !l[1].TargetBlank || l[1].URL != "https://example.invalid/acl" || l[1].Title != "ACL allow/deny" {
		t.Errorf("ACL log link = %+v", l[1])
	}
	if l := Build(Options{ACLLogURL: "https://example.invalid/acl", ACLLogTitle: "Drops"}).Links; l[1].Title != "Drops" {
		t.Errorf("ACL log link title = %q, want Drops", l[1].Title)
	}
}

func TestResourceLatencyOneHour(t *testing.T) {
	p := panelByTitle(t, Build(Options{}), "Resource add/update/delete p99 (1h window)")
	if len(p.Targets) != 3 {
		t.Fatalf("resource latency targets = %+v", p.Targets)
	}
	for i, tg := range p.Targets {
		if !strings.Contains(tg.Expr, "[1h])") || strings.Contains(tg.Expr, "$interval") {
			t.Errorf("resource latency target %d not on a fixed 1h window: %s", i, tg.Expr)
		}
	}
}

func TestDriftTable(t *testing.T) {
	p := panelByTitle(t, Build(Options{}), "Nodes with drift")
	if p.Type != "table" || p.Span != 6 || len(p.Targets) != 3 {
		t.Fatalf("drift table = type %q span %d targets %d", p.Type, p.Span, len(p.Targets))
	}
	withPorts := "max by (node) (max without (instance, pod, endpoint, container, service) (ovnkube_controller_port_group_with_ports))"
	want := []string{
		"ovnk:pg_drift:missing_by_node > 0",
		withPorts + " and on (node) (ovnk:pg_drift:missing_by_node > 0)",
		"(ovnk:pg_drift:missing_by_node > 0) / on (node) clamp_min(" + withPorts + ", 1)",
	}
	for i, tg := range p.Targets {
		if tg.Expr != want[i] {
			t.Errorf("drift target %d = %s\nwant %s", i, tg.Expr, want[i])
		}
	}
}

func TestACLsPerMNPRuleCountsSecondaryNetworksOnly(t *testing.T) {
	p := panelByTitle(t, Build(Options{}), "ACLs per MNP rule")
	if want := `ovnkube_controller_nb_db_objects{table="ACL",owner_type="NetworkPolicy",network!="default"}`; !strings.Contains(p.Targets[0].Expr, want) {
		t.Errorf("numerator must exclude the default network (%s): %s", want, p.Targets[0].Expr)
	}
	if !strings.Contains(p.Description, "secondary/UDN networks") || !strings.Contains(p.Description, "approximates MNP ACLs") {
		t.Errorf("description = %q", p.Description)
	}
	// With no MNPs the panel is legitimately empty; verify-panels must treat
	// it as idle-allowed.
	b, err := os.ReadFile("../verify-panels.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"ACLs per MNP rule"`) {
		t.Error(`hack/verify-panels.sh idle_rule has no "ACLs per MNP rule" entry`)
	}
}

func TestPGDriftStatDescribesDisabledCase(t *testing.T) {
	p := panelByTitle(t, Build(Options{}), "Nodes with PG drift")
	if want := "0 also when --pg-drift=false; check that ovnkube_controller_port_group_with_ports exists"; p.Description != want {
		t.Errorf("description = %q, want %q", p.Description, want)
	}
}
