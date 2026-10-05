# OVN-K Observ — Incident-Driven Amendment — Design

- **Date:** 2026-10-05
- **Status:** Proposed; awaiting review
- **Amends:** [2026-10-02 dashboard design](2026-10-02-ovnk-observ-dashboard-design.md), sections 4, 5, 6, 7, 8, 10, 12
- **Source:** a production incident bridge (OVN-IC, multiple clusters, MultiNetworkPolicy at scale). No customer names, hosts, addresses, or policy names appear here.

## 1. Goal

Make the dashboard answer the questions the incident bridge spent hours answering by hand, without making the exporter or Prometheus noticeably heavier:

1. Are NB and SB consistent on every node, and which nodes need a recompute?
2. Is northd processing incrementally, or stuck recomputing?
3. What is changing the policies, and how fast?
4. How much fan-out does each namespace's policy set carry?
5. Is the cluster itself healthy (node joins, crash loops, version)?

Second goal: restructure the existing dashboard so the first screen answers question 1 and the per-node panels stay readable at 50+ nodes.

## 2. Evidence from the incident

| Observation | Gap in the current dashboard |
|---|---|
| NB Port_Groups with ports had no matching SB Port_Group on some nodes (1–68 missing per node, across several clusters). Found with a shell loop that ran `ovn-nbctl`/`ovn-sbctl list port_group` in every ovnkube-node pod and diffed names. | No NB↔SB consistency metric. |
| Missing counts changed between runs; one cluster "would not stabilize". | A one-shot script cannot show a trend; a time series can. |
| Fix was per node: `ovn-appctl -t ovn-northd inc-engine/recompute`. | No way to see which nodes need it or whether it worked. |
| `inc-engine/show-stats` was read as "zero incremental computes for ACLs". `NB_acl`, `SB_*` and the other `NB_*` rows are input nodes; the engine counts every input-node run as a recompute. The meaningful computed nodes looked very different (`northd` 167 recomputes vs 717k computes; `lflow` ~3% recompute; `sync_from_sb` ~84% recompute). | No curated view of incremental processing, so raw output was misread. |
| Drift lined up with bursts of MNP updates from an external rule-reprocessing job. | No MNP/NAD change rate, no attribution of who writes. |
| The team hand-computed rules and peers per policy group with `jq`. Scale was ~10k MNPs → ~127k Port_Groups, ~231k ACLs. | MNP count and network targets exist, but not rules or peers. |
| Bridge also checked pending node joins, crash-looping pods, and whether the running version had the fix (FDP-3965). | None of these are on the dashboard. |
| An external ACL allow/deny dashboard was used to see drops. | No link out. |

## 3. Decisions

| Decision | Choice | Reason |
|---|---|---|
| Drift detection | Exporter computes it from its existing NB/SB monitors | Same data the bridge script used, with no `exec` and no per-run full dump |
| Name storage | 64-bit FNV-1a hash of the Port_Group name, not the string | At ~500k names the collision probability is ~1e-8; memory drops by roughly 5× |
| Drift detail | Per-node count only; no per-policy label | Policy names are high-cardinality and customer-specific. Operators run the existing script on the flagged node. |
| inc-engine stats | One hard-coded read-only appctl command over the northd control socket | Not exposed as a metric today; the command has no side effects |
| inc-engine scope | Allowlist of computed engine nodes | ~75 nodes × 3 stats per node would be ~225 series per node, mostly input-node noise |
| Churn attribution | Optional `manager` label from `managedFields`: the first N distinct managers keep their own label value for the life of the process and every later manager counts under `_other` (a series cannot move between label values without breaking `rate()`); delete events use `manager="unknown"`, because the cached object names the last writer, not the deleter | Names the writing controller without per-object cardinality |
| New flags default | Drift and inc-engine on; manager label `topN:10` | Each can be turned off for clusters where cost matters |

**Principle change.** Section 8 of the base spec says "the exporter issues monitor requests only and never transacts." That still holds for the OVSDB sockets. The amendment adds one appctl call, `inc-engine/show-stats`, sent to the northd control socket. The command string is a constant; the exporter never sends any other appctl command, and `inc-engine/recompute` is never reachable from the exporter.

## 4. Architecture changes

```
 per node (DaemonSet)                               cluster (Deployment)
 ┌──────────────────────────────────────────┐       ┌───────────────────────────────────┐
 │ NB monitor  + Port_Group.name, .ports    │       │ informers (unchanged set)         │
 │ SB monitor  + Port_Group.name            │       │  + event handlers → churn counter │
 │ drift: NB PGs with ports, no SB name     │       │  + transformMNP keeps rule/peer   │
 │ appctl  ovn-northd inc-engine/show-stats │       │    counts (4 ints per MNP)        │
 └──────────────────────────────────────────┘       └───────────────────────────────────┘
```

**Node mode**

- `pkg/ovsdbmon`: NB `Port_Group` monitors `external_ids`, `name`, `ports`. SB adds `{Name: "Port_Group", Column: "name"}` to `SBTables`.
- `pkg/nbcount`: Port_Group rows additionally keep `nameHash uint64` and `hasPorts bool`. SB Port_Group rows keep the hash of the name with the leading `<tunnel_key>_` prefix stripped; a `map[uint64]uint32` refcounts SB names because one NB Port_Group maps to one SB row per logical switch.
- Drift is computed at scrape time: count NB Port_Groups with `hasPorts` whose hash has SB refcount 0. One pass over the NB Port_Group entries; ~10–20 ms at 500k rows.
- New `pkg/appctl`: finds `ovn-northd.pid` in the mounted run directory, connects to `ovn-northd.<pid>.ctl`, sends `inc-engine/show-stats`, parses `Node:` / `- recompute:` / `- compute:` / `- cancel:` lines. Called from `Collect`, with a 2 s timeout and the last good result cached for one scrape on failure. Uses the existing read-only `/var/run/ovn-ic` mount, the same one the NB/SB sockets come from.
- Drift is suppressed (no series) until both NB and SB report initial sync, so a half-loaded state never shows as drift.

**Cluster mode**

- `pkg/k8scount`: `AddEventHandler` on the NAD, MNP, NP, UDN and CUDN informers increments a counter by resource and op. Informers already use resync 0, so updates are real changes; handlers still skip updates with an unchanged `resourceVersion`.
- `transformMNP` additionally stores ingress/egress rule counts and from/to peer counts as four integers, so the cache still holds no spec.
- Manager attribution reads the most recent `managedFields` entry before the transform drops it.

## 5. Metric catalog additions

| Metric | Type | Labels | Source |
|---|---|---|---|
| `ovnkube_controller_port_group_sb_missing` | gauge | — (node via target) | NB Port_Groups with ports and no SB Port_Group of the same name |
| `ovnkube_controller_port_group_with_ports` | gauge | — | NB Port_Groups with at least one port (denominator) |
| `ovn_northd_inc_engine_runs_total` | counter | `engine_node`, `type` (`recompute`/`compute`/`cancel`) | `inc-engine/show-stats`; allowlist below |
| `ovnkube_clustermanager_object_events_total` | counter | `resource`, `op` (`add`/`update`/`delete`), `manager` | informer event handlers; the first N distinct managers keep their label for the life of the process, later ones count as `_other`, and `unknown` (deletes, objects without managedFields) never uses a slot |
| `ovnkube_clustermanager_multi_network_policy_rules` | gauge | `namespace`, `direction` | Σ ingress / egress rule entries |
| `ovnkube_clustermanager_multi_network_policy_peers` | gauge | `namespace`, `direction` | Σ `from` / `to` peer entries |
| `ovnk_observ_appctl_errors_total` | counter | `command` | exporter self-metric |

**inc-engine allowlist:** `northd`, `lflow`, `port_group`, `sync_to_sb_addr_set`, `sync_from_sb`, `ls_stateful`, `lr_stateful`. Node names absent from the running OVN version are skipped; the list is a flag (`--inc-engine-nodes`) so it can track OVN releases.

The counter resets when northd restarts; `rate()` and `increase()` handle that.

**New flags**

| Flag | Default | Effect |
|---|---|---|
| `--pg-drift` | `true` | Monitor NB `Port_Group.ports` and SB `Port_Group.name`; export drift gauges |
| `--inc-engine` | `true` | Poll northd `inc-engine/show-stats` on scrape |
| `--inc-engine-nodes` | allowlist above | Engine nodes to export |
| `--churn-manager-label` | `topN:10` | `off` drops the `manager` label; `topN:<n>` keeps the first n distinct managers, later ones count as `_other`, `unknown` never uses a slot |

## 6. Dashboard restructure

Rows follow the triage path: consistent? → what is changing? → is processing keeping up? → how big? → DB internals → exporter cost.

| Row | Question | Panels | Change |
|---|---|---|---|
| 0. At a glance | Is it healthy and in sync? | Nodes with PG drift (crit > 0); worst northd recompute ratio; max NB→SB lag; nodes disconnected; retry failures 15m; ovnkube-node restarts 1h; nodes not Ready | Max ACL/PG and largest NB DB move to row 3 |
| 1. Consistency | Which nodes need a recompute? | Table: nodes with drift > 0 (missing, with-ports, ratio); drift over time (topk 10); northd recompute ratio per engine node (topk 10 node×engine); full-recompute events (`increase` of `northd` recompute; marks manual remediation) | **New** |
| 2. Change & churn | What is changing, and who is writing? | Object events/min by resource and op; by `manager`; NB update rate by table/op (moved from row 4); txn failure ratio (moved) | Raw "northd txn rate by result" and "ovn-controller txn rate by result" graphs collapse into row 5 |
| 3. Scale & fan-out | Is this scale? | Max NB ACLs, max PGs, largest NB DB (moved stats); K8s objects; MNP network targets; MNP rules and peers by direction; top networks by ACL/PG (table); ACLs by owner type; SB/NB effectiveness ratio | "NB objects per node by table" becomes a topk(10) table of node×table for ACL and Port_Group only. "NB ACL vs SB Logical_Flow" (2 lines per node) is replaced by `ovnk:nb_sb_effectiveness:ratio`, which is recorded today but unused. New panels "ACLs per MNP rule" and "Port_Groups per MNP". |
| 4. Programming latency | Is OVN-K slow? | Network programming p50/p99 (1h); events/min; pod setup per stage (1h); retry failures by node (topk 10); NB→SB lag / staleness (topk 10) | "Resource add/update/delete p99" moves to a 1h window (it is NaN at `$interval` for the same sparse-event reason as programming latency) |
| 5. OVN internals (collapsed) | Which process is hurting? | northd loop p95/max; build_lflows / nb_db_run / sb_db_run; ovn-controller lflow_run; flow generation/installation; br-int flows; DB size and growth; nbdb/sbdb CPU and RSS; sessions and monitors; connection status; libovsdb disconnects; raw txn rates | Merges old rows 3 and 5 plus the raw txn graphs; collapsed by default |
| 6. Exporter self-cost (collapsed) | What does observing cost? | Exporter CPU and memory; DB connected / initial sync; appctl errors | Adds appctl errors |

**Cluster version.** A table panel (one column, `version` label of `cluster_version{type="current"}`) in row 0, since console singlestats render values, not labels.

**Links.** `hack/dashgen` takes an optional `-acl-log-url` (and `-acl-log-title`) flag that adds a dashboard link to an external ACL allow/deny or NetObserv drop view. Default: no link. Per-ACL drop counting stays out of scope.

**Per-node panels.** Every graph whose legend is `{{node}}` wraps its query in `topk(10, …)` when `$node` is All. Tables replace graphs where the answer is "which nodes".

## 7. Recording rules and alerts

**Recording rules (new)**

| Rule | Expression (sketch) |
|---|---|
| `ovnk:ovn_pod_node:info` | `max by (namespace, pod, node) (kube_pod_info{namespace="openshift-ovn-kubernetes"})` |
| `ovnk:pg_drift:missing_by_node` | `max by (node) (max without (instance, pod, endpoint, container, service) (ovnkube_controller_port_group_sb_missing))` |
| `ovnk:pg_drift:nodes` | `count(ovnk:pg_drift:missing_by_node > 0) or vector(0)` |
| `ovnk:northd_recompute:ratio_15m` | `increase(…{type="recompute"}[15m]) / clamp_min(increase(…{type=~"recompute\|compute"}[15m]), 1)` by node, engine_node |
| `ovnk:northd_full_recompute:increase_5m` | `increase(ovn_northd_inc_engine_runs_total{engine_node="northd",type="recompute"}[5m])` by node |
| `ovnk:object_events:rate_5m` | `sum by (resource, op) (rate(ovnkube_clustermanager_object_events_total[5m]))` |

All existing rules that join to `kube_pod_info` switch to `ovnk:ovn_pod_node:info` (section 9).

**Alerts (new)**

| Alert | Condition | For |
|---|---|---|
| `OVNKPortGroupSBDrift` | `ovnk:pg_drift:missing_by_node > 0` | 10m (transient drift during normal churn clears in seconds) |
| `OVNKNorthdRecomputeHigh` | `ovnk:northd_recompute:ratio_15m{engine_node=~"northd\|lflow"} > 0.5` | 30m |
| `OVNKPolicyChurnHigh` | `sum(ovnk:object_events:rate_5m{resource="multinetworkpolicies",op="update"}) * 60 > 100` | 15m |

Thresholds are starting points; tune them on the lab and on the next incident's data.

## 8. Exporter overhead

| Feature | CPU | Memory | Wire / DB load | Series |
|---|---|---|---|---|
| PG drift | Scrape-time pass, ~10–20 ms per 30 s at 500k NB PGs | +8 B hash + 1 B flag per NB PG; SB refcount and uuid→hash maps ~40–50 B/entry. Estimate 10–25 MB at customer scale. | **Largest cost.** Monitor v1 sends full `ports` sets on every modify (old + new). Initial dump grows by ~40 B per port UUID. Large shared groups (all local pods) resend on every pod churn. | 2 per node |
| inc-engine | One appctl call per scrape; text parse < 1 ms | Negligible | One unix-socket request to northd per 30 s | 7 engine nodes × 3 = 21 per node |
| Churn counter | Negligible | Negligible | None | ≤ 5 resources × 3 ops × 11 managers = 165 |
| Rules/peers | Computed once per event in the transform | +4 ints per MNP | None | 4 per namespace with MNPs |
| Health / version / links | None (existing metrics) | None | None | None |

**Gate for PG drift.** Before enabling by default, measure on the lab (475k NB PGs, mostly empty) and on a synthetic cluster with large shared groups: exporter RSS must stay under the 256 Mi limit with ≥ 30% headroom, and initial sync time must not grow by more than 25%. If either fails, ship with `--pg-drift=false` by default and document it as an incident-time switch.

**Possible follow-up.** Moving Port_Group to `monitor_cond_since` with `update2` diffs would send only port deltas. This is deferred; the minimal client speaks monitor v1 only.

## 9. Query load

Today the dashboard runs 26 `join()` expressions. Each one matches against every `kube_pod_info` series in the cluster, and the dashboard refreshes every minute.

| Change | Effect |
|---|---|
| `ovnk:ovn_pod_node:info` recording rule restricted to `openshift-ovn-kubernetes`; `join()` in `hack/dashgen` and all recording rules use it | Join side shrinks from every pod in the cluster to one or two per node |
| Recording rules for anything used by more than one panel or by an alert (drift, recompute ratio, events) | Panels read precomputed series |
| `topk(10, …)` on per-node graphs when `$node` is All | Fewer series rendered and transferred |
| Collapse rows 5 and 6 by default | Fewer panels on the first screen. Verify on the lab whether the console skips queries for collapsed rows; if it does not, this is a readability change only. |
| Default refresh 1m → 2m | Halves steady-state query load; the data underneath is 30 s scrapes and 1h windows |

`hack/verify-panels.sh` gains a timing column so a panel that is slow against Thanos shows up in review.

## 10. Testing additions

1. **Unit:** drift counting with SB refcounts (one NB PG → several SB rows), name prefix stripping, empty-ports PGs excluded, NB-only and SB-only reconnects, suppression until both sides sync. inc-engine parser against captured `show-stats` output from two OVN versions; unknown engine nodes ignored. Churn handler skips unchanged `resourceVersion`. Transform counts rules and peers.
2. **Integration:** real `ovsdb-server` with NB and SB schemas; insert NB PGs with ports, delete one SB row, assert drift = 1; restore, assert 0.
3. **Rules:** promtool fixtures for each new rule and alert.
4. **Lab:**
   - `ovnkube_controller_port_group_sb_missing` matches the bridge script's per-node output on every node.
   - Exporter RSS and initial sync stay within the section 8 gate.
   - `inc-engine` values match `ovn-appctl -t ovn-northd inc-engine/show-stats` on one node.
   - `verify-panels` reports zero errors and per-panel timings.
5. **Remediation check (requires approval; changes cluster state):** on a node with drift, run `inc-engine/recompute` and confirm the drift gauge drops to 0 and the full-recompute panel shows the event.

## 11. Deferred and out of scope

- **Per-policy drift detail.** An opt-in debug endpoint listing the top missing policy names would need the names kept in memory and would expose them on the metrics port. Deferred; the node-level count plus the existing script is enough.
- **Pods or VMs missing a required label** (`--required-pod-label=<key>`). Needs a pod informer in cluster mode, which costs memory. Evaluating which pods no policy selects is selectors × pods and stays out of scope.
- **ovn-controller inc-engine stats.** Same mechanism, but one more socket per node; add only if the northd view proves useful.
- **Multi-cluster view.** Still out of scope. The new series are small enough to fit an ACM observability allowlist if someone wants a fleet view later.

## 12. Upstream path

- Drift: ovnkube-controller already caches NB and SB Port_Group through libovsdb, so the gauge is a cheap addition there under the same name.
- inc-engine: belongs in OVN itself (northd metrics) or in the ovnkube metrics exporter that already scrapes OVN coverage counters. Propose upstream with the allowlist rationale from section 2.
- Churn and fan-out: ovnkube-cluster-manager under the same names.
