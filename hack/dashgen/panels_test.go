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
	wantTitle     = "Networking / OVN-Kubernetes Scale & Troubleshooting"
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
	allowed := map[string]bool{"graph": true, "singlestat": true, "table": true, "gauge": true, "row": true}
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
