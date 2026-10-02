package main

import "math"

// Schema: the legacy Grafana "rows" layout that the OpenShift console's
// monitoring dashboards page renders (same shape as the dashboards already in
// openshift-config-managed, e.g. grafana-dashboard-ovn-health).

type Dashboard struct {
	Title         string     `json:"title"`
	UID           string     `json:"uid"`
	Tags          []string   `json:"tags"`
	Editable      bool       `json:"editable"`
	Refresh       string     `json:"refresh"`
	SchemaVersion int        `json:"schemaVersion"`
	Timezone      string     `json:"timezone"`
	Time          TimeRange  `json:"time"`
	Templating    Templating `json:"templating"`
	Links         []Link     `json:"links"`
	Rows          []Row      `json:"rows"`
}

type TimeRange struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type Templating struct {
	List []Variable `json:"list"`
}

type Variable struct {
	Name       string   `json:"name"`
	Label      string   `json:"label,omitempty"`
	Type       string   `json:"type"`
	Datasource string   `json:"datasource,omitempty"`
	Query      string   `json:"query"`
	Refresh    int      `json:"refresh"`
	Hide       int      `json:"hide"`
	IncludeAll bool     `json:"includeAll"`
	AllValue   string   `json:"allValue,omitempty"`
	Multi      bool     `json:"multi"`
	Sort       int      `json:"sort,omitempty"`
	Regex      string   `json:"regex"`
	Current    Option   `json:"current"`
	Options    []Option `json:"options"`
}

type Option struct {
	Text     string `json:"text"`
	Value    string `json:"value"`
	Selected bool   `json:"selected"`
}

type Link struct {
	Title       string `json:"title"`
	Type        string `json:"type"`
	URL         string `json:"url"`
	Icon        string `json:"icon"`
	Tooltip     string `json:"tooltip"`
	KeepTime    bool   `json:"keepTime"`
	TargetBlank bool   `json:"targetBlank"`
}

type Row struct {
	Title     string  `json:"title"`
	Collapse  bool    `json:"collapse"`
	Height    string  `json:"height"`
	ShowTitle bool    `json:"showTitle"`
	Panels    []Panel `json:"panels"`
}

type Panel struct {
	ID          int      `json:"id"`
	Title       string   `json:"title"`
	Type        string   `json:"type"`
	Span        int      `json:"span"`
	Datasource  string   `json:"datasource"`
	Description string   `json:"description,omitempty"`
	Targets     []Target `json:"targets"`

	// singlestat. The console colors a singlestat only from
	// options.fieldOptions.thresholds (the legacy thresholds/colors strings
	// are ignored), and renders "gauge" as a singlestat, so neither is used.
	Format    string     `json:"format,omitempty"`
	ValueName string     `json:"valueName,omitempty"`
	Decimals  *int       `json:"decimals,omitempty"`
	Options   *PanelOpts `json:"options,omitempty"`
	ValueMaps []ValueMap `json:"valueMaps,omitempty"`
	Sparkline *Sparkline `json:"sparkline,omitempty"`

	// graph
	Lines         bool    `json:"lines,omitempty"`
	Linewidth     int     `json:"linewidth,omitempty"`
	Fill          int     `json:"fill,omitempty"`
	Stack         bool    `json:"stack,omitempty"`
	NullPointMode string  `json:"nullPointMode,omitempty"`
	Legend        *Legend `json:"legend,omitempty"`
	Tooltip       *Tip    `json:"tooltip,omitempty"`
	XAxis         *XAxis  `json:"xaxis,omitempty"`
	YAxes         []YAxis `json:"yaxes,omitempty"`

	// table
	Styles    []Style `json:"styles,omitempty"`
	Transform string  `json:"transform,omitempty"`
}

type Target struct {
	Expr         string `json:"expr"`
	LegendFormat string `json:"legendFormat"`
	RefID        string `json:"refId"`
	Instant      bool   `json:"instant,omitempty"`
	Format       string `json:"format,omitempty"`

	alias string // table column header for this target's value
}

type PanelOpts struct {
	FieldOptions FieldOptions `json:"fieldOptions"`
}

type FieldOptions struct {
	Thresholds []Threshold `json:"thresholds"`
}

type Threshold struct {
	Color string   `json:"color"`
	Value *float64 `json:"value"`
}

type ValueMap struct {
	Op    string `json:"op"`
	Text  string `json:"text"`
	Value string `json:"value"`
}

type Sparkline struct {
	Show bool `json:"show"`
}

type Legend struct {
	Show         bool `json:"show"`
	AlignAsTable bool `json:"alignAsTable"`
	RightSide    bool `json:"rightSide"`
	Current      bool `json:"current"`
	Max          bool `json:"max"`
	Values       bool `json:"values"`
}

type Tip struct {
	Shared    bool   `json:"shared"`
	Sort      int    `json:"sort"`
	ValueType string `json:"value_type"`
}

type XAxis struct {
	Mode string `json:"mode"`
	Show bool   `json:"show"`
}

type YAxis struct {
	Format  string `json:"format"`
	LogBase int    `json:"logBase"`
	Min     *int   `json:"min,omitempty"`
	Show    bool   `json:"show"`
}

type Style struct {
	Alias      string `json:"alias"`
	Pattern    string `json:"pattern"`
	Type       string `json:"type"`
	Unit       string `json:"unit,omitempty"`
	Decimals   int    `json:"decimals"`
	DateFormat string `json:"dateFormat,omitempty"`
}

const (
	title   = "Networking / OVN-K Observ"
	dsVar   = "$datasource"
	nodeSel = `node=~"$node"`
	ovnNS   = `namespace="openshift-ovn-kubernetes"`

	// PatternFly color names the console understands. There is no pure red;
	// light-red is the most saturated one ("red" renders orange).
	colorOK   = "green"
	colorWarn = "light-yellow"
	colorCrit = "light-red"
)

// join attaches the node label to series that only carry namespace/pod (the
// OVN and ovnkube metrics scraped from openshift-ovn-kubernetes pods) and
// applies the $node filter. max by() guards against duplicate kube_pod_info
// series turning the match into many-to-many.
func join(expr string) string {
	return expr + ` * on (namespace, pod) group_left(node) max by (namespace, pod, node) (kube_pod_info{` + nodeSel + `})`
}

// cluster deduplicates cluster-mode exporter series across a rollout overlap
// (old and new pod both scraped) before they are summed.
func cluster(metric string) string {
	return "max without (instance, pod, endpoint, container, service) (" + metric + ")"
}

func t(expr, legend string) Target { return Target{Expr: expr, LegendFormat: legend} }

// col is a table target whose value column is headed alias.
func col(expr, alias string) Target {
	return Target{Expr: expr, Instant: true, Format: "table", alias: alias}
}

func zero() *int { z := 0; return &z }

func graph(title, unit string, span int, targets ...Target) Panel {
	return Panel{
		Title: title, Type: "graph", Span: span, Targets: targets,
		Lines: true, Linewidth: 1, Fill: 1, NullPointMode: "null",
		Legend:  &Legend{Show: true},
		Tooltip: &Tip{Shared: true, Sort: 2, ValueType: "individual"},
		XAxis:   &XAxis{Mode: "time", Show: true},
		YAxes:   []YAxis{{Format: unit, LogBase: 1, Min: zero(), Show: true}, {Format: "short", LogBase: 1, Show: false}},
	}
}

func stacked(p Panel) Panel { p.Stack = true; p.Fill = 3; return p }

// noMin lets the left y-axis go negative (the console ignores min, but
// Grafana and upstream imports honor it).
func noMin(p Panel) Panel { p.YAxes[0].Min = nil; return p }

func describe(p Panel, d string) Panel { p.Description = d; return p }

func step(color string, v float64) Threshold { return Threshold{Color: color, Value: &v} }

// warnCrit colors value >= warn as warning and >= crit as critical. The
// console picks steps[sortedIndexBy(steps, value) - 1], so a value equal to a
// step's value takes the step below; each step therefore sits at the largest
// float64 below its threshold. The base step is -1 rather than null because
// the console reads null as 0, which would leave a value of 0 uncolored.
func warnCrit(warn, crit float64) []Threshold {
	return []Threshold{
		step(colorOK, -1),
		step(colorWarn, math.Nextafter(warn, math.Inf(-1))),
		step(colorCrit, math.Nextafter(crit, math.Inf(-1))),
	}
}

// stat is a singlestat; steps (may be nil) color it in the console.
func stat(title, unit string, span int, steps []Threshold, expr string) Panel {
	p := Panel{
		Title: title, Type: "singlestat", Span: span, Format: unit,
		ValueName: "current", Targets: []Target{t(expr, "")},
		Sparkline: &Sparkline{Show: false},
	}
	if steps != nil {
		p.Options = &PanelOpts{FieldOptions: FieldOptions{Thresholds: steps}}
	}
	return p
}

func table(title string, span int, labels []string, targets ...Target) Panel {
	styles := []Style{{Alias: "Time", Pattern: "Time", Type: "hidden", DateFormat: "YYYY-MM-DD HH:mm:ss"}}
	for _, l := range labels {
		styles = append(styles, Style{Alias: l, Pattern: l, Type: "string"})
	}
	for i := range targets {
		styles = append(styles, Style{
			Alias: targets[i].alias, Pattern: "Value #" + refID(i), Type: "number", Unit: "short", Decimals: 2,
		})
	}
	styles = append(styles, Style{Alias: "", Pattern: "/.*/", Type: "hidden"})
	return Panel{Title: title, Type: "table", Span: span, Targets: targets, Styles: styles, Transform: "table"}
}

func refID(i int) string { return string(rune('A' + i)) }

func row(title string, panels ...Panel) Row {
	return Row{Title: title, Height: "250px", ShowTitle: true, Panels: panels}
}

func rate(metric string) string { return "rate(" + metric + "[$interval])" }

func sel(metric, matchers string) string { return metric + "{" + matchers + "}" }

// Build returns the full dashboard.
func Build() Dashboard {
	rows := []Row{
		row("At a glance",
			stat("Max NB ACLs on a node", "short", 2, nil, `ovnk:nb_db_objects:max_by_table{table="ACL"}`),
			stat("Max NB PortGroups on a node", "short", 2, nil, `ovnk:nb_db_objects:max_by_table{table="Port_Group"}`),
			stat("Largest NB DB", "bytes", 2, nil, `max(ovn_db_db_size_bytes{db_name="OVN_Northbound"})`),
			amplification(),
			programmingP99(),
			stat("Retry failures 15m", "short", 1, nil, `sum(increase(ovnkube_resource_retry_failures_total[15m]))`),
			// One series per ovnkube-node pod (one per node) with any OVN DB
			// connection down, not one per down connection.
			stat("Nodes disconnected", "short", 1, []Threshold{step(colorOK, -1), step(colorCrit, 0.5)},
				`count(count by (namespace, pod) (ovn_northd_nb_connection_status == 0 or ovn_northd_sb_connection_status == 0 or ovn_controller_southbound_database_connected == 0)) or vector(0)`),
		),
		row("Scale & inventory",
			graph("Kubernetes network objects", "short", 4,
				t(`sum by (managed_by) (`+cluster("ovnkube_clustermanager_network_attachment_definitions")+`)`, "NADs ({{managed_by}})"),
				t(`sum(`+cluster("ovnkube_clustermanager_multi_network_policies")+`)`, "MultiNetworkPolicies"),
				t(`sum(`+cluster("ovnkube_clustermanager_network_policies")+`)`, "NetworkPolicies"),
				t(`sum by (kind) (`+cluster("ovnkube_clustermanager_user_defined_networks")+`)`, "{{kind}}"),
			),
			// Network targets outnumber the other objects by orders of
			// magnitude, so they get their own axis.
			graph("MultiNetworkPolicy network targets", "short", 4,
				t(`sum(`+cluster("ovnkube_clustermanager_multi_network_policy_network_targets")+`)`, "MultiNetworkPolicy network targets")),
			graph("NB objects per node by table", "short", 4,
				t(sel("ovnk:nb_db_objects:sum_by_node_table", nodeSel), "{{node}} {{table}}")),
			table("Top networks by ACL / PortGroup", 4, []string{"network", "table"},
				col(`topk(15, ovnk:nb_db_objects:max_by_network_table{table=~"ACL|Port_Group",network=~"$network"})`, "Objects (max over nodes)")),
			graph("NB ACL vs SB Logical_Flow", "short", 4,
				t(`ovnk:nb_db_objects:sum_by_node_table{table="ACL",`+nodeSel+`}`, "{{node}} NB ACL"),
				t(`sum by (node) (ovnkube_controller_sb_db_objects{table="Logical_Flow",`+nodeSel+`})`, "{{node}} SB Logical_Flow")),
			graph("ACLs by owner type", "short", 4,
				t(`sum by (owner_type) (ovnkube_controller_nb_db_objects{table="ACL",`+nodeSel+`,network=~"$network"})`, "{{owner_type}}")),
		),
		row("Programming latency & backlog",
			graph("Network programming p50/p99", "s", 6,
				t(`histogram_quantile(0.5, sum by (le) (`+rate("ovnkube_controller_network_programming_duration_seconds_bucket")+`))`, "p50"),
				t(`histogram_quantile(0.99, sum by (le) (`+rate("ovnkube_controller_network_programming_duration_seconds_bucket")+`))`, "p99"),
				t(`histogram_quantile(0.99, sum by (le) (`+rate("ovnkube_controller_network_programming_ovn_duration_seconds_bucket")+`))`, "p99 OVN part")),
			// The resource latency histograms carry no kind-like label on this
			// OVN-K version (label_names: le + target labels only), so the
			// split is by operation; J adds the $node filter.
			graph("Resource add/update/delete p99", "s", 6,
				t(`histogram_quantile(0.99, sum by (le) (`+join(rate("ovnkube_controller_resource_add_latency_seconds_bucket"))+`))`, "add"),
				t(`histogram_quantile(0.99, sum by (le) (`+join(rate("ovnkube_controller_resource_update_latency_seconds_bucket"))+`))`, "update"),
				t(`histogram_quantile(0.99, sum by (le) (`+join(rate("ovnkube_controller_resource_delete_latency_seconds_bucket"))+`))`, "delete")),
			describe(graph("Pod setup pipeline p99", "s", 4, t(`ovnk:pod_setup_stage:p99_5m`, "{{stage}}")),
				"p99 per pod-setup stage over 5m. Per-stage p99s are not additive, so the lines are not stacked."),
			graph("Retry failures by node", "short", 4,
				t(`sum by (node) (`+join(rate("ovnkube_resource_retry_failures_total"))+`)`, "{{node}}")),
			graph("NB→SB lag / probe staleness", "s", 4,
				t(sel("ovnk:nb_sb_e2e_lag_seconds", nodeSel), "{{node}} NB→SB lag"),
				t(sel("ovnk:e2e_probe_staleness_seconds", nodeSel), "{{node}} probe staleness")),
		),
		row("NB/SB DB health",
			graph("DB size", "bytes", 6, t(join("ovn_db_db_size_bytes"), "{{node}} {{db_name}}")),
			noMin(graph("DB growth per hour", "bytes", 6,
				t(sel("ovnk:ovn_db_size_bytes:deriv_30m", nodeSel)+` * 3600`, "{{node}} {{db_name}}"))),
			graph("nbdb/sbdb CPU", "short", 4,
				t(`sum by (node, container) (`+rate(sel("container_cpu_usage_seconds_total", ovnNS+`,container=~"nbdb|sbdb",`+nodeSel))+`)`, "{{node}} {{container}}")),
			graph("nbdb/sbdb RSS", "bytes", 4,
				t(`sum by (node, container) (`+sel("container_memory_rss", ovnNS+`,container=~"nbdb|sbdb",`+nodeSel)+`)`, "{{node}} {{container}}")),
			graph("Sessions & monitors", "short", 4,
				t(join("ovn_db_jsonrpc_server_sessions"), "{{node}} {{db_name}} sessions"),
				t(join("ovn_db_ovsdb_monitors"), "{{node}} {{db_name}} monitors")),
			table("Connection status", 6, []string{"node"},
				col(`max by (node) (`+join("ovn_northd_nb_connection_status")+`)`, "northd→NB"),
				col(`max by (node) (`+join("ovn_northd_sb_connection_status")+`)`, "northd→SB"),
				col(`max by (node) (`+join("ovn_controller_southbound_database_connected")+`)`, "ovn-controller→SB")),
			graph("libovsdb disconnects", "short", 6,
				t(`sum by (node) (`+join(rate("ovnkube_master_libovsdb_disconnects_total"))+`)`, "{{node}}")),
		),
		row("Transactions & churn",
			graph("NB update rate by table/op", "ops", 6,
				t(`sum by (table, op) (`+rate(sel("ovnkube_controller_nb_db_updates_total", nodeSel))+`)`, "{{table}} {{op}}")),
			graph("northd txn rate by result", "ops", 6, txnTargets("ovn_northd_txn_")...),
			graph("ovn-controller txn rate by result", "ops", 6, txnTargets("ovn_controller_txn_")...),
			graph("Txn failure ratio", "percentunit", 6,
				t(sel("ovnk:ovn_txn_failure:ratio_5m", nodeSel), "{{node}} {{component}}")),
		),
		row("Recompute cost",
			graph("northd loop p95 / max", "ms", 6,
				t(join("ovn_northd_ovn_northd_loop_95th_percentile"), "{{node}} p95"),
				t(join("ovn_northd_ovn_northd_loop_maximum"), "{{node}} max")),
			graph("northd build_lflows / nb_db_run / sb_db_run p95", "ms", 6,
				t(join("ovn_northd_build_lflows_95th_percentile"), "{{node}} build_lflows"),
				t(join("ovn_northd_ovnnb_db_run_95th_percentile"), "{{node}} nb_db_run"),
				t(join("ovn_northd_ovnsb_db_run_95th_percentile"), "{{node}} sb_db_run")),
			graph("ovn-controller lflow_run rate", "ops", 4,
				t(join(rate("ovn_controller_lflow_run")), "{{node}}")),
			graph("Flow generation / installation p95", "ms", 4,
				t(join("ovn_controller_flow_generation_95th_percentile"), "{{node}} generation"),
				t(join("ovn_controller_flow_installation_95th_percentile"), "{{node}} installation")),
			graph("br-int OpenFlow count", "short", 4,
				t(join("ovn_controller_integration_bridge_openflow_total"), "{{node}}")),
		),
		row("Exporter self-cost",
			graph("Exporter CPU", "short", 3,
				t(`sum by (node, pod) (`+rate(`container_cpu_usage_seconds_total{namespace="ovnk-observ",container="exporter"}`)+`)`, "{{node}} {{pod}}")),
			graph("Exporter memory (working set)", "bytes", 3,
				t(`sum by (node, pod) (container_memory_working_set_bytes{namespace="ovnk-observ",container="exporter"})`, "{{node}} {{pod}}")),
			table("DB connected / initial sync", 6, []string{"node", "db"},
				col(`max by (node, db) (ovnk_observ_db_connected)`, "Connected"),
				col(`max by (node, db) (ovnk_observ_initial_sync_seconds)`, "Initial sync (s)")),
		),
	}
	rows[len(rows)-1].Collapse = true

	id := 1
	for ri := range rows {
		for pi := range rows[ri].Panels {
			p := &rows[ri].Panels[pi]
			p.ID = id
			id++
			p.Datasource = dsVar
			for ti := range p.Targets {
				p.Targets[ti].RefID = refID(ti)
			}
		}
	}

	return Dashboard{
		Title:         title,
		UID:           "ovnk-scale-troubleshooting",
		Tags:          []string{"networking", "ovn-kubernetes", "ovnk-observ"},
		Editable:      false,
		Refresh:       "1m",
		SchemaVersion: 14,
		Timezone:      "UTC",
		Time:          TimeRange{From: "now-1h", To: "now"},
		Templating: Templating{List: []Variable{
			{
				Name: "datasource", Label: "Data source", Type: "datasource", Query: "prometheus", Refresh: 1,
				Current: Option{Text: "default", Value: "default"}, Options: []Option{},
			},
			queryVar("node"),
			queryVar("network"),
			{
				Name: "interval", Type: "interval", Query: "1m,5m,15m",
				Current: Option{Text: "5m", Value: "5m", Selected: true},
				Options: []Option{{Text: "1m", Value: "1m"}, {Text: "5m", Value: "5m", Selected: true}, {Text: "15m", Value: "15m"}},
			},
		}},
		Links: []Link{{
			Title: "Networking / Infrastructure", Type: "link", Icon: "dashboard",
			URL: "/monitoring/dashboards/grafana-dashboard-ovn-health", Tooltip: "OVN health overview", KeepTime: true,
		}},
		Rows: rows,
	}
}

// amplification is NB Port_Groups per policy. The console has no dial, so it
// is a singlestat colored at the same levels as the alert.
func amplification() Panel {
	p := stat("PortGroup amplification", "short", 2, warnCrit(5, 10), `ovnk:portgroup_amplification:ratio`)
	p.Decimals = new(int)
	*p.Decimals = 1
	p.Description = "≥5 warn, ≥10 critical (alert fires >10)"
	return p
}

// programmingP99 shows the recording rule as-is: it is NaN when no
// network-programming events happened in the window, which reads "idle".
func programmingP99() Panel {
	p := stat("Network programming p99", "s", 2, warnCrit(2, 10), `ovnk:network_programming:p99_5m`)
	p.ValueMaps = []ValueMap{{Op: "=", Text: "idle", Value: "NaN"}, {Op: "=", Text: "no data", Value: "null"}}
	p.Description = "p99 over 5m; 'idle' = no network-programming events in the window"
	return p
}

func queryVar(label string) Variable {
	return Variable{
		Name: label, Type: "query", Datasource: dsVar,
		Query:   "label_values(ovnkube_controller_nb_db_objects, " + label + ")",
		Refresh: 2, IncludeAll: true, AllValue: ".*", Sort: 1,
		Current: Option{Text: "All", Value: "$__all"}, Options: []Option{},
	}
}

func txnTargets(prefix string) []Target {
	var ts []Target
	for _, r := range []string{"success", "try_again", "error", "aborted"} {
		ts = append(ts, t(`sum(`+join(rate(prefix+r))+`)`, r))
	}
	return ts
}
