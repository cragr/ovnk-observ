package main

import (
	"maps"
	"slices"
)

// Schema: the current Grafana "panels" + gridPos layout, for the ACM hub
// Grafana. The console dashboard (panels.go) keeps the legacy "rows" layout
// because that is the only one the OpenShift console renders.

const (
	hubCMName    = "grafana-dashboard-ovn-fleet-triage"
	hubCMNS      = "open-cluster-management-observability"
	hubDataKey   = "ovn-fleet-triage.json"
	hubUID       = "ovnk-observ-fleet"
	hubFolder    = "OVN-K"
	hubClusterRE = `cluster=~"$cluster"`
)

// hubDS points every panel at the $datasource variable (default: ACM's
// Observatorium datasource).
var hubDS = DSRef{Type: "prometheus", UID: "${datasource}"}

type HubDashboard struct {
	Title         string        `json:"title"`
	UID           string        `json:"uid"`
	Tags          []string      `json:"tags"`
	Editable      bool          `json:"editable"`
	Refresh       string        `json:"refresh"`
	SchemaVersion int           `json:"schemaVersion"`
	Timezone      string        `json:"timezone"`
	Time          TimeRange     `json:"time"`
	Templating    HubTemplating `json:"templating"`
	Panels        []HubPanel    `json:"panels"`
}

type HubTemplating struct {
	List []HubVariable `json:"list"`
}

type HubVariable struct {
	Name       string `json:"name"`
	Label      string `json:"label"`
	Type       string `json:"type"`
	Datasource *DSRef `json:"datasource,omitempty"`
	Query      string `json:"query"`
	Refresh    int    `json:"refresh"`
	IncludeAll bool   `json:"includeAll"`
	AllValue   string `json:"allValue,omitempty"`
	Multi      bool   `json:"multi"`
	Sort       int    `json:"sort,omitempty"`
	Current    Option `json:"current"`
}

type DSRef struct {
	Type string `json:"type"`
	UID  string `json:"uid"`
}

type GridPos struct {
	H int `json:"h"`
	W int `json:"w"`
	X int `json:"x"`
	Y int `json:"y"`
}

type HubPanel struct {
	ID              int              `json:"id"`
	Title           string           `json:"title"`
	Type            string           `json:"type"`
	GridPos         GridPos          `json:"gridPos"`
	Collapsed       *bool            `json:"collapsed,omitempty"`
	Datasource      *DSRef           `json:"datasource,omitempty"`
	Description     string           `json:"description,omitempty"`
	Targets         []HubTarget      `json:"targets,omitempty"`
	FieldConfig     *FieldConfig     `json:"fieldConfig,omitempty"`
	Options         map[string]any   `json:"options,omitempty"`
	Transformations []Transformation `json:"transformations,omitempty"`
	Panels          []HubPanel       `json:"panels"` // rows only; always empty (rows are expanded)
}

type HubTarget struct {
	Datasource   DSRef  `json:"datasource"`
	Expr         string `json:"expr"`
	LegendFormat string `json:"legendFormat,omitempty"`
	RefID        string `json:"refId"`
	Instant      bool   `json:"instant,omitempty"`
	Range        bool   `json:"range"`
	Format       string `json:"format,omitempty"`
}

type FieldConfig struct {
	Defaults  FieldDefaults `json:"defaults"`
	Overrides []Override    `json:"overrides"`
}

type FieldDefaults struct {
	Unit       string         `json:"unit,omitempty"`
	Decimals   *int           `json:"decimals,omitempty"`
	Min        *float64       `json:"min,omitempty"`
	Thresholds *HubThresholds `json:"thresholds,omitempty"`
	Color      *FieldColor    `json:"color,omitempty"`
	NoValue    string         `json:"noValue,omitempty"`
}

type FieldColor struct {
	Mode string `json:"mode"`
}

type HubThresholds struct {
	Mode  string      `json:"mode"`
	Steps []Threshold `json:"steps"`
}

type Override struct {
	Matcher    Matcher    `json:"matcher"`
	Properties []Property `json:"properties"`
}

type Matcher struct {
	ID      string `json:"id"`
	Options string `json:"options"`
}

type Property struct {
	ID    string `json:"id"`
	Value any    `json:"value"`
}

type DataLink struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

type Transformation struct {
	ID      string         `json:"id"`
	Options map[string]any `json:"options"`
}

// hubLayout assigns ids and gridPos. Each row is a full-width row panel
// followed by its panels laid out left to right at a fixed height.
type hubLayout struct {
	id, y  int
	panels []HubPanel
}

func (l *hubLayout) next() int { l.id++; return l.id }

func (l *hubLayout) row(title string, h int, ps ...HubPanel) {
	f := false
	l.panels = append(l.panels, HubPanel{ID: l.next(), Title: title, Type: "row", Collapsed: &f,
		GridPos: GridPos{H: 1, W: 24, Y: l.y}, Panels: []HubPanel{}})
	l.y++
	x := 0
	for _, p := range ps {
		p.ID = l.next()
		p.GridPos = GridPos{H: h, W: p.GridPos.W, X: x, Y: l.y}
		x += p.GridPos.W
		l.panels = append(l.panels, p)
	}
	l.y += h
}

func hubPanel(typ, title string, w int, targets ...HubTarget) HubPanel {
	for i := range targets {
		targets[i].RefID = refID(i)
		targets[i].Datasource = hubDS
	}
	ds := hubDS
	return HubPanel{Type: typ, Title: title, GridPos: GridPos{W: w}, Datasource: &ds, Targets: targets,
		FieldConfig: &FieldConfig{Overrides: []Override{}}}
}

func instant(expr string) HubTarget { return HubTarget{Expr: expr, Instant: true, Format: "table"} }

// hubSteps colors value >= warn as warning and >= crit as critical; a null
// base step covers everything below. warn <= 0 omits the warning step.
func hubSteps(warn, crit float64) []Threshold {
	s := []Threshold{{Color: colorOK}}
	if warn > 0 {
		s = append(s, step(colorWarn, warn))
	}
	return append(s, step(colorCrit, crit))
}

func hubDescribe(p HubPanel, d string) HubPanel { p.Description = d; return p }

func hubStat(title, unit string, w int, steps []Threshold, expr string) HubPanel {
	p := hubPanel("stat", title, w, HubTarget{Expr: expr, Instant: true})
	p.FieldConfig.Defaults = FieldDefaults{Unit: unit, Color: &FieldColor{Mode: "thresholds"},
		Thresholds: &HubThresholds{Mode: "absolute", Steps: steps}, NoValue: "0"}
	p.Options = map[string]any{"colorMode": "background", "graphMode": "none", "textMode": "value",
		"reduceOptions": map[string]any{"calcs": []string{"lastNotNull"}, "fields": "", "values": false}}
	return p
}

func hubSeries(title, unit string, w int, expr string) HubPanel {
	p := hubPanel("timeseries", title, w, HubTarget{Expr: expr, LegendFormat: "{{cluster}}", Range: true})
	p.FieldConfig.Defaults = FieldDefaults{Unit: unit, Min: new(float64)}
	p.Options = map[string]any{"legend": map[string]any{"displayMode": "list", "placement": "bottom", "showLegend": true},
		"tooltip": map[string]any{"mode": "multi", "sort": "desc"}}
	return p
}

// fields keeps only the named fields, renames them, and sorts by the first
// renamed column, descending.
func fields(rename map[string]string, keep []string, sortBy string) []Transformation {
	include := append([]string{}, keep...)
	for _, k := range slices.Sorted(maps.Keys(rename)) {
		include = append(include, k)
	}
	return []Transformation{
		{ID: "filterFieldsByName", Options: map[string]any{"include": map[string]any{"names": include}}},
		{ID: "organize", Options: map[string]any{"renameByName": rename}},
		{ID: "sortBy", Options: map[string]any{"sort": []map[string]any{{"field": sortBy, "desc": true}}}},
	}
}

func unitOverride(field, unit string) Override {
	return Override{Matcher: Matcher{ID: "byName", Options: field}, Properties: []Property{{ID: "unit", Value: unit}}}
}

func hc(matchers string) string {
	if matchers == "" {
		return "{" + hubClusterRE + "}"
	}
	return "{" + hubClusterRE + "," + matchers + "}"
}

var (
	hubRatio        = "ovnk:northd_recompute:ratio_15m" + hc(`engine_node=~"northd|lflow"`)
	hubFullRecomp1h = "increase(ovn_northd_inc_engine_runs_total" + hc(`engine_node="northd",type="recompute"`) + "[1h])"
	hubAppctlErr1h  = "increase(ovnk_observ_appctl_errors_total" + hc("") + "[1h])"
	hubDriftNodes   = "ovnk:pg_drift:nodes" + hc("")
)

// BuildHub returns the ACM hub fleet-triage dashboard: which clusters, and
// which nodes in them, are unhealthy or need an inc-engine/recompute.
func BuildHub() HubDashboard {
	crit1 := hubSteps(0, 1)
	var l hubLayout
	l.row("Fleet at a glance", 4,
		hubStat("Clusters reporting", "none", 4, []Threshold{{Color: "blue"}},
			"count(count by (cluster) ("+hubDriftNodes+"))"),
		hubStat("Clusters with PG drift", "none", 4, crit1,
			"count("+hubDriftNodes+" > 0) or vector(0)"),
		hubStat("Nodes with PG drift", "none", 4, crit1,
			"sum("+hubDriftNodes+") or vector(0)"),
		hubDescribe(hubStat("Worst northd recompute ratio", "percentunit", 4, hubSteps(0.2, 0.5),
			"max("+hubRatio+")"),
			"Share of northd and lflow engine runs over 15m that were full recomputes, worst node in the selected clusters."),
		hubDescribe(hubStat("Full recomputes 1h", "none", 4, hubSteps(1, 5),
			"sum("+hubFullRecomp1h+") or vector(0)"),
			"northd engine full recomputes. Includes manual inc-engine/recompute and northd restarts."),
		hubDescribe(hubStat("Clusters with appctl errors 1h", "none", 4, crit1,
			"count(sum by (cluster) ("+hubAppctlErr1h+") > 0) or vector(0)"),
			"Clusters where the exporter could not read inc-engine/show-stats on some node; their inc-engine series are incomplete."),
	)

	clusters := hubPanel("table", "Clusters", 24,
		instant("sum by (cluster) ("+hubDriftNodes+")"),
		instant("max by (cluster) ("+hubRatio+")"),
		instant("sum by (cluster) ("+hubFullRecomp1h+")"),
		instant("sum by (cluster) ("+hubAppctlErr1h+")"),
	)
	clusters.Description = "One row per cluster, worst first. Click a cluster to filter the dashboard to it."
	clusters.Transformations = append([]Transformation{{ID: "merge", Options: map[string]any{}}},
		fields(map[string]string{
			"Value #A": "Nodes with PG drift",
			"Value #B": "Worst recompute ratio",
			"Value #C": "Full recomputes 1h",
			"Value #D": "appctl errors 1h",
		}, []string{"cluster"}, "Nodes with PG drift")...)
	clusters.FieldConfig.Overrides = []Override{
		{Matcher: Matcher{ID: "byName", Options: "cluster"}, Properties: []Property{{ID: "links", Value: []DataLink{{
			Title: "Filter to ${__value.raw}",
			URL:   "/d/" + hubUID + "?var-cluster=${__value.raw}&${__url_time_range}&${datasource:queryparam}",
		}}}}},
		unitOverride("Worst recompute ratio", "percentunit"),
		unitOverride("Full recomputes 1h", "none"),
		unitOverride("appctl errors 1h", "none"),
	}
	l.row("Clusters", 8, clusters)

	nodes := hubPanel("table", "Nodes needing recompute", 24,
		instant("ovnk:pg_drift:missing_by_node"+hc("")+" > 0"))
	nodes.Description = "Nodes whose SB is missing NB Port_Groups. Fix: ovn-appctl -t ovn-northd inc-engine/recompute on that node."
	nodes.Transformations = fields(map[string]string{"Value": "Missing Port_Groups"}, []string{"cluster", "node"}, "Missing Port_Groups")
	l.row("Nodes needing recompute", 8, nodes)

	l.row("Trends", 8,
		hubSeries("Nodes with PG drift by cluster", "none", 8,
			"topk(10, sum by (cluster) ("+hubDriftNodes+"))"),
		hubSeries("Worst northd recompute ratio by cluster", "percentunit", 8,
			"topk(10, max by (cluster) ("+hubRatio+"))"),
		hubDescribe(hubSeries("Full-recompute events by cluster", "none", 8,
			"topk(10, sum by (cluster) (ovnk:northd_full_recompute:increase_5m"+hc("")+"))"),
			"northd full recomputes per 5m window. Includes manual inc-engine/recompute and northd restarts."),
	)

	return HubDashboard{
		Title:         "OVN-K Observ / Fleet triage",
		UID:           hubUID,
		Tags:          []string{"ovn-kubernetes", "ovnk-observ", "acm"},
		Refresh:       "2m",
		SchemaVersion: 39,
		Timezone:      "browser",
		Time:          TimeRange{From: "now-6h", To: "now"},
		Templating: HubTemplating{List: []HubVariable{
			{Name: "datasource", Label: "Datasource", Type: "datasource", Query: "prometheus",
				Current: Option{Text: "Observatorium", Value: "Observatorium"}},
			{Name: "cluster", Label: "Cluster", Type: "query", Datasource: &hubDS,
				Query: "label_values(ovnk:pg_drift:nodes, cluster)", Refresh: 2, IncludeAll: true, AllValue: ".*",
				Multi: true, Sort: 1, Current: Option{Text: "All", Value: "$__all"}},
		}},
		Panels: l.panels,
	}
}
