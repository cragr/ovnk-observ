package main

import (
	"bufio"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func hubPanels(d HubDashboard) []HubPanel {
	var ps []HubPanel
	for _, p := range d.Panels {
		if p.Type != "row" {
			ps = append(ps, p)
		}
	}
	return ps
}

func hubPanelByTitle(t *testing.T, d HubDashboard, title string) HubPanel {
	t.Helper()
	for _, p := range hubPanels(d) {
		if p.Title == title {
			return p
		}
	}
	t.Fatalf("no hub panel titled %q", title)
	return HubPanel{}
}

func TestHubRows(t *testing.T) {
	var got []string
	for _, p := range BuildHub().Panels {
		if p.Type == "row" {
			got = append(got, p.Title)
		}
	}
	want := []string{"Fleet at a glance", "Clusters", "Nodes needing recompute", "Trends"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("hub rows = %q, want %q", got, want)
	}
}

func TestHubPanelsValid(t *testing.T) {
	allowed := map[string]bool{"row": true, "stat": true, "table": true, "timeseries": true}
	d := BuildHub()
	seen := map[int]string{}
	rowY, rowW := -1, 0
	for _, p := range d.Panels {
		if prev, ok := seen[p.ID]; ok {
			t.Errorf("panel %q reuses id %d of %q", p.Title, p.ID, prev)
		}
		seen[p.ID] = p.Title
		if p.ID <= 0 {
			t.Errorf("panel %q has non-positive id %d", p.Title, p.ID)
		}
		if !allowed[p.Type] {
			t.Errorf("panel %q has type %q", p.Title, p.Type)
		}
		g := p.GridPos
		if g.W <= 0 || g.H <= 0 || g.X < 0 || g.X+g.W > 24 {
			t.Errorf("panel %q has gridPos %+v", p.Title, g)
		}
		if p.Type == "row" {
			continue
		}
		if g.Y != rowY {
			rowY, rowW = g.Y, 0
		}
		rowW += g.W
		if rowW > 24 {
			t.Errorf("panels at y=%d are wider than 24 (at %q)", g.Y, p.Title)
		}
		if len(p.Targets) == 0 {
			t.Errorf("panel %q has no targets", p.Title)
		}
		for _, tg := range p.Targets {
			if !strings.Contains(tg.Expr, `cluster=~"$cluster"`) {
				t.Errorf("panel %q target %s does not filter on $cluster: %s", p.Title, tg.RefID, tg.Expr)
			}
			if tg.Datasource != hubDS {
				t.Errorf("panel %q target %s datasource = %+v", p.Title, tg.RefID, tg.Datasource)
			}
		}
	}
}

func TestHubVariables(t *testing.T) {
	d := BuildHub()
	if len(d.Templating.List) != 2 {
		t.Fatalf("want 2 variables, got %d", len(d.Templating.List))
	}
	ds, cl := d.Templating.List[0], d.Templating.List[1]
	if ds.Name != "datasource" || ds.Type != "datasource" || ds.Query != "prometheus" || ds.Current.Value != "Observatorium" {
		t.Errorf("datasource variable = %+v", ds)
	}
	if cl.Name != "cluster" || !cl.Multi || !cl.IncludeAll || cl.Query != "label_values(ovnk:pg_drift:nodes, cluster)" {
		t.Errorf("cluster variable = %+v", cl)
	}
}

// Every OVN-K metric a hub panel reads must be forwarded by ACM; a missing
// allowlist entry shows up only as an empty panel on the hub.
func TestHubMetricsAllowlisted(t *testing.T) {
	f, err := os.Open("../../deploy/acm/metrics-allowlist.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	allow := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if n, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "- "); ok {
			allow[n] = true
		}
	}
	metric := regexp.MustCompile(`\bovnk?[_:][a-zA-Z0-9_:]*`)
	for _, p := range hubPanels(BuildHub()) {
		for _, tg := range p.Targets {
			for _, m := range metric.FindAllString(tg.Expr, -1) {
				if !allow[m] {
					t.Errorf("panel %q reads %s, which is not in deploy/acm/metrics-allowlist.yaml", p.Title, m)
				}
			}
		}
	}
}

func TestHubClusterTableLinksToCluster(t *testing.T) {
	p := hubPanelByTitle(t, BuildHub(), "Clusters")
	for _, o := range p.FieldConfig.Overrides {
		if o.Matcher.Options != "cluster" {
			continue
		}
		for _, prop := range o.Properties {
			if prop.ID != "links" {
				continue
			}
			links := prop.Value.([]DataLink)
			if len(links) == 1 && strings.Contains(links[0].URL, "var-cluster=${__value.raw}") {
				return
			}
		}
	}
	t.Fatal("Clusters table has no data link on the cluster column that sets var-cluster")
}

func TestHubConfigMap(t *testing.T) {
	y, err := HubConfigMapYAML(BuildHub())
	if err != nil {
		t.Fatal(err)
	}
	s := string(y)
	for _, want := range []string{
		"  name: grafana-dashboard-ovn-fleet-triage\n",
		"  namespace: open-cluster-management-observability\n",
		"    grafana-custom-dashboard: \"true\"\n",
		"    observability.open-cluster-management.io/dashboard-folder: OVN-K\n",
		"data:\n  ovn-fleet-triage.json: |-\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("hub configmap missing %q", want)
		}
	}
}

// The committed hub artifacts must match the generator; run 'make dashboard'.
func TestHubGeneratedFilesCurrent(t *testing.T) {
	d := BuildHub()
	j, err := HubJSON(d)
	if err != nil {
		t.Fatal(err)
	}
	y, err := HubConfigMapYAML(d)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string][]byte{
		"../../dashboards/" + hubDataKey:            j,
		"../../deploy/acm/dashboard-configmap.yaml": y,
	} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Errorf("%s is stale; run 'make dashboard'", path)
		}
	}
}
