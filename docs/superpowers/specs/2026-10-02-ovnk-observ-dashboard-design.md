# OVN-Kubernetes Scale & Troubleshooting Dashboard — Design

- **Date:** 2026-10-02
- **Status:** Approved in brainstorming; awaiting spec review
- **Target platform:** OpenShift 4.20+ (lab: 5.0.0-rc.4, 3 control-plane + 2 workers), OVN Interconnect topology

## 1. Goal

Give SREs and support engineers one console dashboard under **Observe → Dashboards** that answers, top to bottom:

1. Is OVN-Kubernetes unhealthy?
2. Is the cause scale (NADs, MultiNetworkPolicies, ACLs, PortGroups)?
3. Where is programming slow (control plane, NB→SB propagation, ovn-controller)?
4. Which database or process is hurting (size, transactions, recomputes, disconnects)?

The work ships upstream eventually. We prototype in a lab first.

**Success criteria**

- The dashboard exposes the NB object explosion on the lab (Section 2) within one screen.
- Exporter counts match `ovsdb-client` counts exactly.
- Every panel query returns data without error.
- New metric names and labels port unchanged to ovn-kubernetes.

## 2. Motivating evidence (lab, 2026-10-02)

The lab reproduces a support case: 149 NADs and 3,150 MultiNetworkPolicies, all in namespace `<repro-namespace>`.

Local NB/SB on `<worker-node>`:

| Object | Count |
|---|---|
| NB ACL | 464,102 |
| NB Port_Group | 474,791 |
| NB Logical_Switch_Port | 172 |
| SB Logical_Flow | 1,562 |
| NB DB file | 954 MB |
| SB DB file | 79 MB |

PortGroup names such as `repro-vlan3126-network-controller:NetworkPolicy:<repro-namespace>:repro-br-1452-ingress` show that each MNP is instantiated once per secondary-network controller (3,150 × ~149 ≈ 470k). Almost none of that state reaches SB. `/etc/ovn/libovsdb.log` rotates about once a minute, which suggests heavy churn.

No current metric shows any of this. The existing "Networking / Infrastructure" dashboard (`grafana-dashboard-ovn-health`) covers only CNI latency, connectivity checks, and CPU/RSS.

## 3. Decisions

| Decision | Choice | Reason |
|---|---|---|
| Topology | OVN Interconnect only | 4.20+; per-node NB/SB, no RAFT |
| Audience | Upstream/product, prototyped in lab | Missing metrics belong in ovnkube-controller |
| Dashboard format | Classic console ConfigMap (Grafana JSON) in `openshift-config-managed` | Same mechanism CNO uses; no operator install |
| Prototype exporter | Go, minimal streaming OVSDB client (node mode) + client-go (cluster mode) | libovsdb materializes the full initial dump; see Section 13 |
| Counting strategy | Custom update handler over the streaming client, no cache | Full cache costs hundreds of MB at lab scale |

## 4. Architecture

```
 per node (DaemonSet)                         cluster (Deployment, 1 replica)
 ┌───────────────────────────────┐            ┌──────────────────────────────┐
 │ ovnk-observ-exporter          │            │ ovnk-observ-exporter         │
 │   --mode=node                 │            │   --mode=cluster             │
 │   monitor NB/SB (read-only)   │            │   informers: NAD, MNP, NP,   │
 │   via /var/run/ovn/*.sock     │            │   UDN, CUDN, Namespace       │
 └──────────────┬────────────────┘            └──────────────┬───────────────┘
                │ :9410/metrics                              │ :9410/metrics
                └──────────────► ServiceMonitors ◄───────────┘
                                      │
                    platform Prometheus (+ PrometheusRule)
                                      │
                       console dashboard ConfigMap
```

**Components**

1. **`ovnk-observ-exporter`**: one Go binary.
   - `--mode=node`: DaemonSet in namespace `ovnk-observ`. Mounts host `/var/run/ovn`. Opens read-only monitors (`pkg/ovsdbmon`, minimal streaming JSON-RPC client) on the local `ovnnb_db.sock` and `ovnsb_db.sock`, requesting only `_uuid`, `external_ids`, and `name` where needed.
   - `--mode=cluster`: Deployment with informers over Kubernetes objects.
2. **ServiceMonitors** scraped by platform Prometheus. The namespace carries `openshift.io/cluster-monitoring=true`.
3. **PrometheusRule** with recording rules and alerts (Section 7).
4. **Dashboard ConfigMap** `grafana-dashboard-ovn-scale-troubleshooting`, label `console.openshift.io/dashboard=true`.

## 5. Metric catalog

### 5.1 New metrics (upstream names)

| Metric | Type | Labels | Source |
|---|---|---|---|
| `ovnkube_controller_nb_db_objects` | gauge | `table`, `owner_type`, `network` | NB monitor; `owner_type` from `k8s.ovn.org/owner-type`, `network` from `k8s.ovn.org/owner-controller` minus the `-network-controller` suffix (`default` when absent) |
| `ovnkube_controller_sb_db_objects` | gauge | `table` | SB monitor: Logical_Flow, Port_Binding, Datapath_Binding, MAC_Binding, FDB |
| `ovnkube_controller_nb_db_updates_total` | counter | `table`, `op` (insert/modify/delete) | NB monitor update stream; stands in for transactions per second |
| `ovnkube_clustermanager_network_attachment_definitions` | gauge | `namespace`, `managed_by` (`user`/`udn`) | informer |
| `ovnkube_clustermanager_multi_network_policies` | gauge | `namespace` | informer |
| `ovnkube_clustermanager_multi_network_policy_network_targets` | gauge | `namespace` | informer; Σ over MNPs of entries in the `k8s.v1.cni.cncf.io/policy-for` annotation |
| `ovnkube_clustermanager_network_policies` | gauge | `namespace` | informer |
| `ovnkube_clustermanager_user_defined_networks` | gauge | `kind` (UDN/CUDN), `topology`, `role` | informer |

NB tables counted: ACL, Port_Group, Address_Set, Logical_Switch_Port, Logical_Switch, Logical_Router, Logical_Router_Port, Load_Balancer, Load_Balancer_Group.

### 5.2 Exporter self-metrics (prototype only)

`ovnk_observ_db_connected{db}`, `ovnk_observ_initial_sync_seconds{db}`, `ovnk_observ_informer_synced{resource}`, plus standard `process_*` and `go_*`.

### 5.3 Cardinality guard

`--per-network-labels=topN:50` (default) keeps the 50 largest networks per table and folds the rest into `network="_other"`. `--per-network-labels=off` drops the label. Gauges reset to zero on reconnect and never carry stale values.

### 5.4 Existing metrics used

Verified present on the lab:

- **Programming latency:** `ovnkube_controller_network_programming_duration_seconds`, `..._network_programming_ovn_duration_seconds`, `..._resource_{add,update,delete}_latency_seconds`, the `..._pod_*_duration_seconds` pipeline, `ovnkube_resource_retry_failures_total`.
- **Propagation:** `ovnkube_controller_nb_e2e_timestamp`, `ovnkube_controller_sb_e2e_timestamp`, `ovn_db_e2e_timestamp`.
- **DB health:** `ovn_db_db_size_bytes`, `ovn_db_jsonrpc_server_sessions`, `ovn_db_ovsdb_monitors`, `ovn_northd_{nb,sb}_connection_status`, `ovn_northd_status`, `ovn_controller_southbound_database_connected`, `ovnkube_master_libovsdb_disconnects_total`, container CPU/RSS for `nbdb` and `sbdb`.
- **Transactions:** `ovn_northd_txn_*`, `ovn_controller_txn_*` (success, try_again, error, aborted, incomplete, unchanged, uncommitted).
- **Recompute cost:** `ovn_northd_ovn_northd_loop_*`, `ovn_northd_build_lflows_*`, `ovn_northd_ovnnb_db_run_*`, `ovn_northd_ovnsb_db_run_*`, `ovn_controller_lflow_run`, `ovn_controller_flow_generation_*`, `ovn_controller_flow_installation_*`, `ovn_controller_integration_bridge_openflow_total`.
- **K8s scale:** `ovnkube_controller_admin_network_policies_db_objects`, `ovnkube_controller_num_egress_firewall_rules`.

Confirmed: existing OVN metrics carry `pod`, not `node`; join with `* on (namespace, pod) group_left(node) kube_pod_info`. `ovn_*_txn_*` are counters (use `rate`). `ovn_db_*` carry `db_name` (`OVN_Northbound`/`OVN_Southbound`).

## 6. Dashboard

**Title:** Networking / OVN-K Observ
**Variables:** `$node`, `$network` (`label_values(ovnkube_controller_nb_db_objects, network)`), `$interval`
**Links:** "Networking / Infrastructure" for CNI latency and CPU/RSS. This dashboard does not duplicate them.

| Row | Question | Panels |
|---|---|---|
| 0. At a glance | Is something wrong? | Max NB ACLs on any node; max PortGroups; largest NB DB; PortGroup amplification; p99 network programming; retry failures (15m); nodes with SB or northd disconnected |
| 1. Scale & inventory | Is this scale? | K8s objects over time (NAD user/UDN, MNP, MNP network targets, NP, UDN/CUDN); NB objects per node by table; top 15 networks by ACL and PortGroup (table); NB ACL vs SB Logical_Flow; ACLs by `owner_type` |
| 2. Programming latency & backlog | Is OVN-K slow? | Network programming p50/p99 by kind (+ `_ovn_`), fixed 1h window; network programming events/min by kind; resource add/update/delete p99 by kind; pod setup pipeline per-stage p99 over 1h (not stacked: per-stage p99s are not additive); retry failure rate by node; NB→SB lag and e2e staleness per node |
| 3. NB/SB DB health | Is ovsdb-server hurting? | DB size by node and DB; size growth (`deriv` 30m); nbdb/sbdb CPU and RSS; sessions; monitors; connection status; libovsdb disconnect rate |
| 4. Transactions & churn | How busy is the DB? | NB update rate by table and op; northd and ovn-controller txn rate by result; txn failure ratio |
| 5. Recompute cost | Are we stuck rebuilding? | northd loop p95/max; `build_lflows`; `ovnnb_db_run`/`ovnsb_db_run`; ovn-controller `lflow_run` rate; flow generation/installation p95/max; br-int OpenFlow count |
| 6. Exporter self-cost (collapsed) | What does observing cost? | Exporter CPU/RSS per node; DB connected; initial sync time |

Panel types: graph, singlestat, table, row. The console `monitoring-plugin` renders `gauge` with the same SingleStat component (no dial), so it is not used; singlestats are colored via `options.fieldOptions.thresholds`.

## 7. Recording rules and alerts

One PrometheusRule, group `ovnk-observ.rules`, interval 30s.

**Recording rules**

| Rule | Expression (sketch) |
|---|---|
| `ovnk:nb_db_objects:max_by_table` | `max by (table) (sum by (instance, table) (ovnkube_controller_nb_db_objects))` |
| `ovnk:nb_db_objects:sum_by_network_table` | per-network totals on the worst node |
| `ovnk:portgroup_amplification:ratio` | NetworkPolicy-owned PortGroups (worst node) ÷ (sum MNP + sum NP) |
| `ovnk:nb_sb_effectiveness:ratio` | SB Logical_Flow ÷ NB ACL, per node |
| `ovnk:network_programming:p99_1h` | `histogram_quantile(0.99, …[1h])` (events are sparse; 5m windows give NaN) |
| `ovnk:network_programming:p99_1h_by_kind` | same, `sum by (le, kind)` |
| `ovnk:pod_setup_stage:p99_1h` | one series per pipeline stage over `[1h]`, label `stage` |
| `ovnk:ovn_txn_failure:ratio_5m` | (error + try_again + aborted) ÷ total, label `component` |
| `ovnk:nb_sb_e2e_lag_seconds` | `ovnkube_controller_nb_e2e_timestamp - ovnkube_controller_sb_e2e_timestamp`, per node |
| `ovnk:e2e_probe_staleness_seconds` | `time() - ovnkube_controller_nb_e2e_timestamp`, per node (normal < ~60s) |
| `ovnk:ovn_db_size_bytes:deriv_30m` | `deriv(ovn_db_db_size_bytes[30m])` |

**Alerts** (prototype, `severity=warning`, `for: 15m`; thresholds are tunable parameters)

| Alert | Condition |
|---|---|
| `OVNKPortGroupAmplificationHigh` | amplification > 10 |
| `OVNKNBDBObjectsHigh` | any node ACL or PortGroup count > 100,000 |
| `OVNKNBDBGrowing` | NB size growth > 50 MB/h sustained 1h |
| `OVNKPropagationLagHigh` | NB→SB lag > 30s |
| `OVNKE2EProbeStale` | e2e staleness > 180s |
| `OVNKTxnFailureRatioHigh` | failure ratio > 5% |
| `OVNKNetworkProgrammingSlow` | p99 > 10s |

CNO does not ship alerts. Upstream, the alerts become an optional separate PR or a doc.

## 8. Failure handling

- **Memory.** Node mode skips the libovsdb cache and decodes the dump row by row. A custom update handler keeps `map[uuid] → {table, ownerTypeID, networkID}` with interned strings, about 30–50 MB for ~940k rows.
- **Initial dump.** The first monitor on a ~1 GB NB loads ovsdb-server once. The exporter logs the duration, exports `ovnk_observ_initial_sync_seconds`, and backs off exponentially (max 5m) between reconnects so crash loops cannot hammer the DB.
- **Socket missing or DB restart.** Set `ovnk_observ_db_connected=0`, drop object gauges, reconnect with backoff. Panels show gaps, not wrong numbers.
- **Informers not synced.** Cluster mode emits no object series until first sync; `ovnk_observ_informer_synced` reports state.
- **Resources.** Node DaemonSet requests 50m/64Mi, limits 500m/256Mi. Its priority class sits below `system-node-critical` so the kubelet evicts it before OVN.
- **Read-only.** The exporter issues monitor requests only and never transacts.

## 9. Repository layout

```
cmd/ovnk-observ-exporter/main.go     # --mode, --per-network-labels, --listen
pkg/nbcount/                          # update handler + counters; no k8s deps
pkg/k8scount/                         # informers for NAD/MNP/NP/UDN/CUDN
pkg/metrics/                          # metric definitions, cardinality guard
pkg/ovsdbmon/                         # minimal streaming OVSDB JSON-RPC monitor client
deploy/                               # kustomize base: namespace, RBAC, DaemonSet, Deployment,
                                      # Services, ServiceMonitors, PrometheusRule (quay.io image)
overlays/dev/                         # base + BuildConfig, ImageStream (in-cluster dev image)
install/ovnk-observ.yaml              # rendered by make install-manifest; what admins apply
dashboards/ovn-scale-troubleshooting.json
hack/verify-panels.sh                 # runs every panel query against thanos-querier
Makefile, Containerfile, go.mod
```

- Go version matches ovn-kubernetes' `go.mod` where practical; node mode needs no libovsdb dependency.
- Build: `make image-build` / `make image-push` publish `quay.io/cragr/ovnk-observ-exporter:<version>` (base `ubi9/ubi-micro:9.6`). For development, `make image-dev` runs a binary `BuildConfig` into the in-cluster registry.
- `make deploy` (dev overlay), `make deploy-release` (install manifest), `make dashboard`, `make install-manifest`, `make undeploy` (removes everything, including the dashboard).

## 10. Testing

1. **Unit** (`pkg/nbcount`, `pkg/metrics`): synthetic insert/modify/delete updates, owner-type changes, unknown networks, top-N folding into `_other`, reset on reconnect.
2. **Integration:** real `ovsdb-server` with the OVN NB schema (`ovsdb-tool create`); insert N ACLs and PortGroups; assert `/metrics`.
3. **Rules:** `promtool check rules` and `promtool test rules` with fixtures for every recording rule and alert.
4. **Lab verification:**
   - Exporter ACL and PortGroup counts on `<worker-node>` equal `ovsdb-client` counts (baseline 464,102 / 474,791 at time of measurement; re-measure at test time).
   - `hack/verify-panels.sh` reports zero errors and non-empty results for every panel.
   - Exporter RSS stays under 256 Mi.
   - Dashboard renders in the console (browser screenshot).
5. **Scale delta** (requires user approval; mutates the repro): delete a batch of MNPs and confirm PortGroups and ACLs drop by ~149 per MNP.

## 11. Upstream path

1. ovn-kubernetes PR: `nb_db_objects`, `sb_db_objects`, `nb_db_updates_total` computed from ovnkube-controller's existing libovsdb cache; cluster-manager K8s object gauges.
2. CNO PR: dashboard ConfigMap in bindata beside `grafana-dashboard-ovn-health`.
3. Optional: alerts as a separate PR or doc.

## 12. Out of scope

Perses dashboards; server-side ovsdb transaction rate (needs `ovsdb-server` changes); per-ACL hit or drop statistics; alert routing; multi-cluster views; pre-4.20 RAFT topology.

## 13. Amendments (2026-10-02, from plan research)

- **§3/§4/§8/§9: no libovsdb in node mode.** Node mode uses a minimal streaming OVSDB JSON-RPC client (`pkg/ovsdbmon`). Reason: libovsdb materializes the full initial dump (~940k rows). `pkg/ovsdbmodel` is dropped.
- **§5.1: `policy_for` label replaced.** `ovnkube_clustermanager_multi_network_policies{namespace}` loses `policy_for`; new `ovnkube_clustermanager_multi_network_policy_network_targets{namespace}` = Σ over MNPs of entries in the `k8s.v1.cni.cncf.io/policy-for` annotation. Reason: on the lab every MNP lists all 147 NADs (one 3k-char label value). 3,150 × 147 ≈ 463k explains the PortGroup count as configured fan-out.
- **§5.4: existing-metric facts confirmed.** OVN metrics carry `pod`, not `node`; join with `* on (namespace, pod) group_left(node) kube_pod_info`. `ovn_*_txn_*` are counters (use `rate`). `ovn_db_*` carry `db_name` (`OVN_Northbound`/`OVN_Southbound`).
- **§7: lag definitions.** NB→SB lag = `ovnkube_controller_nb_e2e_timestamp - ovnkube_controller_sb_e2e_timestamp`; write-loop staleness = `time() - ovnkube_controller_nb_e2e_timestamp` (normal < ~60s). `OVNKPropagationLagHigh` uses NB→SB lag > 30s; new `OVNKE2EProbeStale` fires on staleness > 180s. Reason: the old controller-minus-DB lag did not match the series that exist.
- **§6: panel types and MNP panel.** `gauge` added to allowed panel types; dashboard adds "MNP network targets" next to MNP count.
- 2026-10-02: latency rules moved to 1h windows (sparse events → NaN at 5m); node-mode series deduped before summing (rollout overlap doubled counts).
