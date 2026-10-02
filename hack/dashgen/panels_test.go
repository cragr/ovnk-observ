package main

import (
	"bufio"
	"bytes"
	"encoding/json"
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
	d := Build()
	want := []string{
		"At a glance",
		"Scale & inventory",
		"Programming latency & backlog",
		"NB/SB DB health",
		"Transactions & churn",
		"Recompute cost",
		"Exporter self-cost",
	}
	var got []string
	for _, r := range d.Rows {
		got = append(got, r.Title)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("row titles = %q, want %q", got, want)
	}
	for i, r := range d.Rows {
		last := i == len(d.Rows)-1
		if r.Collapse != last {
			t.Errorf("row %q collapse = %v, want %v", r.Title, r.Collapse, last)
		}
	}
}

func TestPanelsValid(t *testing.T) {
	// "gauge" renders as a plain singlestat in the console, so it is not used.
	allowed := map[string]bool{"graph": true, "singlestat": true, "table": true, "row": true}
	seen := map[int]string{}
	d := Build()
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
	want := `ovn_db_db_size_bytes * on (namespace, pod) group_left(node) max by (namespace, pod, node) (kube_pod_info{node=~"$node"})`
	if got != want {
		t.Fatalf("join = %s\nwant  %s", got, want)
	}
}

func TestTitleAndVariables(t *testing.T) {
	d := Build()
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
	d := Build()
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

	// Pull the block scalar back out and compare it with Build().
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
		t.Fatal("configmap JSON differs from Build() output")
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
	for _, r := range Build().Rows {
		n += len(r.Panels)
	}
	if n != 37 {
		t.Errorf("panel count = %d, want 37", n)
	}
}

func TestSinglestatColorsUseFieldOptions(t *testing.T) {
	for _, r := range Build().Rows {
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
	d := Build()
	cases := []struct {
		title string
		want  map[float64]string
	}{
		{"PortGroup amplification", map[float64]string{0: "green", 4.9: "green", 5: "light-yellow", 9.99: "light-yellow", 10: "light-red", 143.6: "light-red"}},
		{"Network programming p99", map[float64]string{0: "green", 1.5: "green", 2: "light-yellow", 9.5: "light-yellow", 10: "light-red", 30: "light-red"}},
		{"Nodes disconnected", map[float64]string{0: "green", 1: "light-red", 3: "light-red"}},
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

func TestProgrammingP99ValueMaps(t *testing.T) {
	p := panelByTitle(t, Build(), "Network programming p99")
	want := []ValueMap{{Op: "=", Text: "idle", Value: "NaN"}, {Op: "=", Text: "no data", Value: "null"}}
	if !reflect.DeepEqual(p.ValueMaps, want) {
		t.Errorf("valueMaps = %+v, want %+v", p.ValueMaps, want)
	}
	if p.Targets[0].Expr != "ovnk:network_programming:p99_5m" {
		t.Errorf("p99 expr = %q, want the plain recording rule", p.Targets[0].Expr)
	}
	if !strings.Contains(p.Description, "'idle' = no network-programming events") {
		t.Errorf("p99 description = %q", p.Description)
	}
}

func TestScaleRowLayout(t *testing.T) {
	d := Build()
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
	for _, title := range []string{"Kubernetes network objects", "NB objects per node by table"} {
		if p := panelByTitle(t, d, title); p.Span != 4 {
			t.Errorf("%q span = %d, want 4", title, p.Span)
		}
	}
}

func TestExporterSelfCostSplit(t *testing.T) {
	d := Build()
	for title, unit := range map[string]string{"Exporter CPU": "short", "Exporter memory (working set)": "bytes"} {
		p := panelByTitle(t, d, title)
		if p.Type != "graph" || p.Span != 3 || p.YAxes[0].Format != unit || len(p.Targets) != 1 {
			t.Errorf("%q = type %q span %d unit %q targets %d", title, p.Type, p.Span, p.YAxes[0].Format, len(p.Targets))
			continue
		}
		if !strings.HasPrefix(p.Targets[0].Expr, "sum by (node, pod) (") || p.Targets[0].LegendFormat != "{{node}} {{pod}}" {
			t.Errorf("%q target = %+v", title, p.Targets[0])
		}
	}
	if p := panelByTitle(t, d, "DB connected / initial sync"); p.Span != 6 {
		t.Errorf("exporter table span = %d, want 6", p.Span)
	}
}

func TestGraphYAxisMin(t *testing.T) {
	for _, r := range Build().Rows {
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
	p := panelByTitle(t, Build(), "Pod setup pipeline p99")
	if p.Stack {
		t.Error("per-stage p99s are not additive; panel must not stack")
	}
	if !strings.Contains(p.Description, "not additive") {
		t.Errorf("description = %q", p.Description)
	}
}
