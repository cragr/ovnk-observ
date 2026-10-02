# OVN-Kubernetes Scale & Troubleshooting Dashboard Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship a prototype exporter, recording rules/alerts, and a console dashboard that expose OVN-Kubernetes NB/SB object scale, DB health, transaction churn, and recompute cost on OpenShift 4.20+.

**Architecture:** One Go binary (`ovnk-observ-exporter`) runs as a per-node DaemonSet (streaming read-only OVSDB monitor on the local NB/SB unix sockets, compact counters) and as a 1-replica cluster Deployment (dynamic informers for NAD/MNP/NP/UDN/CUDN). Platform Prometheus scrapes both; a PrometheusRule precomputes rollups; a Go generator emits the classic console dashboard ConfigMap.

**Tech Stack:** Go 1.26, `github.com/prometheus/client_golang v1.23.2`, `k8s.io/client-go v0.36.2` (+ `k8s.io/api`, `k8s.io/apimachinery` at matching v0.36.2), kustomize, promtool 3.5.0, OpenShift BuildConfig (binary, Docker strategy).

**Spec:** `docs/superpowers/specs/2026-10-02-ovnk-observ-dashboard-design.md` (amended by Task 1 — read the amended version).

## Global Constraints

- Module path: `github.com/cragr/ovnk-observ`. `go 1.26.0` in `go.mod`.
- New metric names exactly: `ovnkube_controller_nb_db_objects{table,owner_type,network}`, `ovnkube_controller_sb_db_objects{table}`, `ovnkube_controller_nb_db_updates_total{table,op}`, `ovnkube_clustermanager_network_attachment_definitions{namespace,managed_by}`, `ovnkube_clustermanager_multi_network_policies{namespace}`, `ovnkube_clustermanager_multi_network_policy_network_targets{namespace}`, `ovnkube_clustermanager_network_policies{namespace}`, `ovnkube_clustermanager_user_defined_networks{kind,topology,role}`.
- Self metrics exactly: `ovnk_observ_db_connected{db}`, `ovnk_observ_initial_sync_seconds{db}`, `ovnk_observ_informer_synced{resource}`.
- `db` label values: `nb`, `sb`. `op` values: `insert`, `modify`, `delete`.
- NB tables: `ACL`, `Port_Group`, `Address_Set`, `Logical_Switch_Port`, `Logical_Switch`, `Logical_Router`, `Logical_Router_Port`, `Load_Balancer`, `Load_Balancer_Group`. SB tables: `Logical_Flow`, `Port_Binding`, `Datapath_Binding`, `MAC_Binding`, `FDB`.
- `owner_type` = `external_ids["k8s.ovn.org/owner-type"]`, `"none"` if absent. `network` = `external_ids["k8s.ovn.org/owner-controller"]` with suffix `-network-controller` stripped; `"default"` if absent or empty.
- Default `--per-network-labels=topN:50`; fold the rest into `network="_other"`; `off` drops the label (emits `network=""`).
- Listen `:9410`, paths `/metrics`, `/healthz`.
- Namespace `ovnk-observ`, label `openshift.io/cluster-monitoring: "true"`.
- Node DaemonSet resources: requests `50m`/`64Mi`, limits `500m`/`256Mi`.
- Reconnect backoff: exponential, start 1s, factor 2, cap 5m.
- The exporter never sends `transact`; only `monitor` and `echo` replies.
- Dashboard ConfigMap `grafana-dashboard-ovn-scale-troubleshooting` in `openshift-config-managed`, label `console.openshift.io/dashboard: "true"`, data key `ovn-scale-troubleshooting.json`. Title `Networking / OVN-Kubernetes Scale & Troubleshooting`. Panel types limited to `graph`, `singlestat`, `table`, `gauge`, `row` (types already rendered by this cluster's console).
- Lab: node arch `amd64`; NB schema 7.18.0, SB schema 21.8.0; socket paths `/var/run/ovn/ovnnb_db.sock`, `/var/run/ovn/ovnsb_db.sock`.

## Review Focus

1. **ovsdb-server `echo` requests** — the client must answer them with the same params and id, or the server drops the connection mid-dump. Test: `TestMonitorRepliesToEcho` (Task 3).
2. **Initial dump of ~940k rows** — decoding must stream row by row; heap after GC must stay below 96 MiB for 500k synthetic rows. Test: `TestMonitorStreamsLargeDumpWithinMemory` (Task 3).
3. **`external_ids` edge encodings** — `["map",[]]`, missing owner-controller, owner-controller without the suffix, non-string garbage — yield `default`/`none`/value-as-is, never a panic. Test: `TestParseExternalIDs` table cases (Task 2).
4. **Socket absent at startup or DB restart** — `ovnk_observ_db_connected` goes to 0, object series disappear (not stale), reconnect resumes with backoff. Tests: `TestCollectorOmitsObjectsWhenDisconnected` (Task 4), `TestRunReconnectsAfterServerClose` (Task 3).
5. **Modify that changes owner/network, delete of unknown UUID** — counts move buckets and never go negative. Test: `TestCounterModifyMovesBucket`, `TestCounterDeleteUnknownIsNoop` (Task 2).

## File Structure

```
go.mod, go.sum, Makefile, Dockerfile, .gitignore
cmd/ovnk-observ-exporter/main.go        # flags, mode wiring, HTTP server
pkg/nbcount/labels.go                   # ParseExternalIDs → (ownerType, network)
pkg/nbcount/counter.go                  # compact per-table store + update counters
pkg/ovsdbmon/client.go                  # streaming OVSDB JSON-RPC monitor client
pkg/ovsdbmon/decode.go                  # stream decoder for table-updates objects
pkg/metrics/nbcollector.go              # prometheus.Collector for NB/SB + self metrics
pkg/metrics/topn.go                     # topN folding + flag parsing
pkg/k8scount/collector.go               # cluster-mode informers + collector
hack/integration/                       # real ovsdb-server test run in-cluster
hack/dashgen/main.go, panels.go         # dashboard JSON generator
hack/verify-panels.sh                   # runs every panel query against thanos-querier
dashboards/ovn-scale-troubleshooting.json   # generated, checked in
deploy/                                 # kustomize tree (Task 7)
rules/ovnk-observ-rules.yaml, rules/tests/*.yaml
```

---

### Task 1: Scaffold module and amend spec

**Files:**
- Create: `go.mod`, `Makefile`, `.gitignore`, `cmd/ovnk-observ-exporter/main.go` (stub)
- Modify: `docs/superpowers/specs/2026-10-02-ovnk-observ-dashboard-design.md`

**Interfaces:**
- Produces: `make test` (`go test ./...`), `make build` (`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/ovnk-observ-exporter ./cmd/ovnk-observ-exporter`), `make tools` (downloads promtool 3.5.0 for host OS/arch into `bin/`).

- [ ] **Step 1: Amend the spec.** Add a section "13. Amendments (2026-10-02, from plan research)" and update the referenced sections:
  - §4/§9: node mode uses a minimal streaming OVSDB JSON-RPC client (`pkg/ovsdbmon`), not libovsdb, because libovsdb materializes the full initial dump; drop `pkg/ovsdbmodel`.
  - §5.1: replace `policy_for` label with `ovnkube_clustermanager_multi_network_policy_network_targets{namespace}` = Σ over MNPs of entries in the `k8s.v1.cni.cncf.io/policy-for` annotation. Reason: on the lab every MNP lists all 147 NADs (one 3k-char label value); 3,150 × 147 ≈ 463k explains the PortGroup count as configured fan-out.
  - §5.4: existing OVN metrics carry `pod`, not `node`; join with `* on (namespace, pod) group_left(node) kube_pod_info`. `ovn_*_txn_*` are counters (use `rate`). `ovn_db_*` carry `db_name` (`OVN_Northbound`/`OVN_Southbound`).
  - §7: NB→SB lag = `ovnkube_controller_nb_e2e_timestamp - ovnkube_controller_sb_e2e_timestamp`; write-loop staleness = `time() - ovnkube_controller_nb_e2e_timestamp` (normal < ~60s). Alert `OVNKPropagationLagHigh` uses NB→SB lag > 30s; add `OVNKE2EProbeStale` for staleness > 180s.
  - §6: add `gauge` to allowed panel types; dashboard adds "MNP network targets" next to MNP count.
- [ ] **Step 2: Scaffold.** `go mod init github.com/cragr/ovnk-observ`; set `go 1.26.0`; stub `main.go` that exits 0. `.gitignore`: `bin/`.
- [ ] **Step 3: Verify.** Run: `make build && make test && make tools && bin/promtool --version`. Expected: binary built, `no test files`, promtool prints `3.5.0`.
- [ ] **Step 4: Commit.** `git add -A && git commit -m "chore: scaffold module; amend spec from plan research"`

---

### Task 2: External-ID parsing and compact counter (`pkg/nbcount`)

**Files:**
- Create: `pkg/nbcount/labels.go`, `pkg/nbcount/counter.go`
- Test: `pkg/nbcount/labels_test.go`, `pkg/nbcount/counter_test.go`

**Interfaces:**
- Produces:
  - `func ParseExternalIDs(m map[string]string) (ownerType, network string)`
  - `type Key struct{ OwnerType, Network string }`
  - `type Counter struct` (safe for concurrent use; one per DB)
  - `func NewCounter() *Counter`
  - `func (c *Counter) Upsert(table string, uuid [16]byte, k Key) (op string)` — returns `"insert"` or `"modify"`
  - `func (c *Counter) Delete(table string, uuid [16]byte) bool`
  - `func (c *Counter) RecordUpdate(table, op string)` — increments update counters (callers skip during initial dump)
  - `func (c *Counter) Reset()` — clears rows, keeps update counters
  - `func (c *Counter) Snapshot() map[string]map[Key]int` — table → key → count
  - `func (c *Counter) Updates() map[[2]string]uint64` — {table, op} → total
  - `func ParseUUID(s string) ([16]byte, error)`

- [ ] **Step 1: Write failing tests.**
  - `TestParseExternalIDs` table:
    - `{"k8s.ovn.org/owner-type":"NetworkPolicy","k8s.ovn.org/owner-controller":"repro-vlan3126-network-controller"}` → `("NetworkPolicy","repro-vlan3126")`
    - `{"k8s.ovn.org/owner-controller":"default-network-controller"}` → `("none","default")`
    - `{}` and `nil` → `("none","default")`
    - `{"k8s.ovn.org/owner-controller":"custom"}` → `("none","custom")`
    - `{"k8s.ovn.org/owner-controller":""}` → `("none","default")`
  - `TestCounterInsertSnapshot`: 3 ACL upserts across two keys → snapshot counts 2 and 1; ops all `insert`.
  - `TestCounterModifyMovesBucket`: upsert uuid U with key A then key B → returns `modify`; snapshot A=0 absent, B=1.
  - `TestCounterDeleteUnknownIsNoop`: `Delete` of unseen UUID returns false; snapshot unchanged; no negative values.
  - `TestCounterResetKeepsUpdates`: after `RecordUpdate("ACL","insert")` and `Reset()`, `Snapshot()` is empty and `Updates()[{"ACL","insert"}]==1`.
  - `TestParseUUID`: `"a5048458-9663-8742-9012-000000000000"` round-trips; `"bad"` errors.
- [ ] **Step 2: Run** `go test ./pkg/nbcount/ -v`. Expected: FAIL (undefined symbols).
- [ ] **Step 3: Implement.** Store per table `map[[16]byte]uint32` where the value indexes an interned `[]Key` slice (`map[Key]uint32` for lookup) and a parallel `[]int` count per (table, keyIndex). Guard with one `sync.Mutex`. Snapshot omits zero counts.
- [ ] **Step 4: Run** `go test ./pkg/nbcount/ -race -v`. Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -am "feat(nbcount): external-id parsing and compact counter"` (add new files first).

---

### Task 3: Streaming OVSDB monitor client (`pkg/ovsdbmon`)

**Files:**
- Create: `pkg/ovsdbmon/client.go`, `pkg/ovsdbmon/decode.go`
- Test: `pkg/ovsdbmon/client_test.go` (fake server over `net.Pipe` / temp unix socket)

**Interfaces:**
- Consumes: `nbcount.Counter`, `nbcount.ParseExternalIDs`, `nbcount.ParseUUID`, `nbcount.Key`.
- Produces:
  - `type TableSpec struct{ Name string; Column string; KeyFromRow bool }` — `Column` is the single monitored column; `KeyFromRow` true → derive `Key` from the `external_ids` map, false → `Key{OwnerType:"none",Network:"default"}`.
  - `type Config struct{ Socket, Database string; Tables []TableSpec; Counter *nbcount.Counter; OnState func(connected bool, initialSync time.Duration) }`
  - `func Run(ctx context.Context, cfg Config) error` — loops connect → monitor → stream → on error `Counter.Reset()`, `OnState(false,0)`, backoff (1s ×2, cap 5m, reset to 1s after a connection that lasted ≥ 1m); returns `ctx.Err()` on cancel.
  - Package vars `NBTables`, `SBTables []TableSpec`: NB tables use `Column:"external_ids", KeyFromRow:true`; SB: `Logical_Flow`→`table_id`, `Port_Binding`→`logical_port`, `Datapath_Binding`→`tunnel_key`, `MAC_Binding`→`logical_port`, `FDB`→`dp_key`, all `KeyFromRow:false`.

**Wire protocol (RFC 7047):** request `{"method":"monitor","params":[<db>,"ovnk-observ",{<table>:{"columns":[<col>]}}],"id":1}`. Reply `{"result":{<table>:{<uuid>:{"new":{...}}}},"error":null,"id":1}`. Notifications `{"method":"update","params":["ovnk-observ",<table-updates>],"id":null}`: insert = `new` only, delete = `old` only, modify = both. Echo `{"method":"echo","params":P,"id":X}` → reply `{"result":P,"error":null,"id":X}`. Maps encode as `["map",[["k","v"],...]]`.

- [ ] **Step 1: Write failing tests.**
  - `TestMonitorInitialDumpCounts`: fake server replies with 3 ACLs (2 owned by `repro-vlan3000-network-controller`, 1 with `["map",[]]`) → counter snapshot `ACL{NetworkPolicy,repro-vlan3000}=2`, `ACL{none,default}=1`; `OnState(true, d)` called with `d>0`; `Updates()` empty.
  - `TestMonitorAppliesUpdates`: after dump, server sends one insert, one modify (owner change), one delete → snapshot reflects them; `Updates()` = insert 1, modify 1, delete 1.
  - `TestMonitorRepliesToEcho`: server sends `{"method":"echo","params":["x"],"id":"echo"}` mid-stream → client writes `{"id":"echo","result":["x"],"error":null}` within 1s.
  - `TestMonitorStreamsLargeDumpWithinMemory`: server writes a 500,000-row ACL dump (each row 4 external_ids keys) generated on the fly; after dump `runtime.GC()`, `MemStats.HeapAlloc < 96<<20`; snapshot total = 500,000.
  - `TestRunReconnectsAfterServerClose`: server closes after dump → `OnState(false,0)` called and snapshot empty; listener accepts a second connection within 3s (inject backoff start via unexported `testBackoffStart = 10*time.Millisecond`).
  - `TestMonitorNeverSendsTransact`: record every request method the client sends during the above → only `monitor` and echo replies.
- [ ] **Step 2: Run** `go test ./pkg/ovsdbmon/ -v`. Expected: FAIL.
- [ ] **Step 3: Implement `decode.go`.** Use `json.Decoder.Token()` to walk each incoming message: read top-level keys; when the key is `result` (id 1) or the second element of `params` for `update`, walk table → uuid → row-update object, `Decode` each row-update into `struct{ Old, New map[string]json.RawMessage }`, apply to the counter, and discard. Never `Decode` a whole `result`. Parse the map column from `json.RawMessage` into `map[string]string`; non-map values → empty map.
- [ ] **Step 4: Implement `client.go`.** `net.Dial("unix", Socket)`, one writer mutex for requests/echo replies, `Run` loop per Interfaces. `OnState(true, time.Since(start))` after the reply finishes.
- [ ] **Step 5: Run** `go test ./pkg/ovsdbmon/ -race -v`. Expected: PASS.
- [ ] **Step 6: Commit.** `git add pkg/ovsdbmon && git commit -m "feat(ovsdbmon): streaming read-only OVSDB monitor client"`

---

### Task 4: Node-mode Prometheus collector (`pkg/metrics`)

**Files:**
- Create: `pkg/metrics/topn.go`, `pkg/metrics/nbcollector.go`
- Test: `pkg/metrics/topn_test.go`, `pkg/metrics/nbcollector_test.go`

**Interfaces:**
- Consumes: `nbcount.Counter`, `nbcount.Key`.
- Produces:
  - `type NetworkLabelMode struct{ Off bool; TopN int }`
  - `func ParseNetworkLabelMode(s string) (NetworkLabelMode, error)` — accepts `off`, `topN:<n>` with n ≥ 1
  - `func FoldTopN(counts map[string]int, n int) map[string]int` — keeps the n largest by total; ties broken by name ascending; remainder summed under `_other` (omitted if zero)
  - `type DBState struct` with `func (s *DBState) Set(connected bool, initialSync time.Duration)`
  - `func NewNodeCollector(nb, sb *nbcount.Counter, nbState, sbState *DBState, mode NetworkLabelMode) prometheus.Collector`

- [ ] **Step 1: Write failing tests.**
  - `TestParseNetworkLabelMode`: `"topN:50"`→`{TopN:50}`, `"off"`→`{Off:true}`, `"topN:0"`, `"top:5"`, `""` → error.
  - `TestFoldTopN`: `{a:5,b:3,c:3,d:1}`, n=2 → `{a:5,b:3,_other:4}`; n=10 → unchanged.
  - `TestNodeCollectorEmitsObjects`: NB counter with ACL keys `{NetworkPolicy,net1}=2`, `{none,default}=1`; SB counter `Logical_Flow`=4 → `testutil.CollectAndCompare` matches `ovnkube_controller_nb_db_objects{network="net1",owner_type="NetworkPolicy",table="ACL"} 2`, `...{network="default",owner_type="none",table="ACL"} 1`, `ovnkube_controller_sb_db_objects{table="Logical_Flow"} 4`, `ovnk_observ_db_connected{db="nb"} 1`.
  - `TestNodeCollectorTopNFoldsPerTable`: 60 networks with ACLs, mode topN:50 → 51 distinct `network` values for `table="ACL"`, sum unchanged.
  - `TestNodeCollectorOffMode`: mode off → `network=""` and counts summed per (table, owner_type).
  - `TestCollectorOmitsObjectsWhenDisconnected`: `nbState.Set(false,0)` → no `ovnkube_controller_nb_db_objects` series, `ovnk_observ_db_connected{db="nb"} 0` present.
  - `TestNodeCollectorUpdatesCounter`: `RecordUpdate("ACL","delete")` twice → `ovnkube_controller_nb_db_updates_total{op="delete",table="ACL"} 2`.
- [ ] **Step 2: Run** `go test ./pkg/metrics/ -v`. Expected: FAIL.
- [ ] **Step 3: Implement.** Custom `Collect` builds const metrics from `Snapshot()`/`Updates()` on each scrape; `ovnk_observ_initial_sync_seconds{db}` emitted once known. Topn folding applied per (table, owner_type) group over network totals.
- [ ] **Step 4: Run** `go test ./pkg/metrics/ -race -v`. Expected: PASS.
- [ ] **Step 5: Commit.** `git add pkg/metrics && git commit -m "feat(metrics): node collector with top-N network folding"`

---

### Task 5: Cluster-mode Kubernetes collector (`pkg/k8scount`)

**Files:**
- Create: `pkg/k8scount/collector.go`
- Test: `pkg/k8scount/collector_test.go` (uses `k8s.io/client-go/dynamic/fake` and `kubernetes/fake`)

**Interfaces:**
- Produces:
  - `func NewCollector(ctx context.Context, dyn dynamic.Interface, kube kubernetes.Interface) (prometheus.Collector, error)` — starts informers, returns immediately.
  - GVRs: NAD `k8s.cni.cncf.io/v1/network-attachment-definitions`, MNP `k8s.cni.cncf.io/v1beta1/multi-networkpolicies`, UDN `k8s.ovn.org/v1/userdefinednetworks`, CUDN `k8s.ovn.org/v1/clusteruserdefinednetworks`; NetworkPolicy via typed informer.
  - Rules: NAD `managed_by="udn"` if any ownerReference `apiVersion` starts with `k8s.ovn.org/`, else `"user"`. MNP targets = count of non-empty comma-separated entries in annotation `k8s.v1.cni.cncf.io/policy-for`. UDN `topology` = `spec.topology`, `role` = `spec.layer2.role` or `spec.layer3.role` (`""` if absent); CUDN reads the same under `spec.network`. `kind` = `UDN` or `CUDN`.
  - `ovnk_observ_informer_synced{resource}` with resource ∈ `nad`, `mnp`, `networkpolicy`, `udn`, `cudn`; object series for a resource are omitted until it syncs. A CRD that does not exist (UDN on old clusters) → synced stays 0, no error.

- [ ] **Step 1: Write failing tests.**
  - `TestCollectorCountsNADsByOwner`: 2 user NADs in `default`, 1 UDN-owned in `tenant-a` → `..._network_attachment_definitions{managed_by="user",namespace="default"} 2`, `{managed_by="udn",namespace="tenant-a"} 1`.
  - `TestCollectorCountsMNPTargets`: 2 MNPs in `repro` with policy-for `"default/a,default/b,default/c"` and `"default/a"` → `multi_network_policies{namespace="repro"} 2`, `multi_network_policy_network_targets{namespace="repro"} 4`.
  - `TestCollectorCountsUDNs`: one UDN Layer2 Primary, one CUDN Layer3 Secondary → two series with `kind`, `topology`, `role` as given.
  - `TestCollectorCountsNetworkPolicies`: 3 NPs across 2 namespaces → per-namespace counts.
  - `TestCollectorMissingCRDNotFatal`: fake dynamic client without UDN resources → NewCollector returns nil error; `ovnk_observ_informer_synced{resource="udn"} 0`.
  - Tests wait for sync with `assert.Eventually`-style polling (2s).
- [ ] **Step 2: Run** `go test ./pkg/k8scount/ -v`. Expected: FAIL.
- [ ] **Step 3: Implement** with `dynamicinformer.NewDynamicSharedInformerFactory(dyn, 0)`; counts computed from listers at scrape time.
- [ ] **Step 4: Run** `go test ./pkg/k8scount/ -race -v`. Expected: PASS.
- [ ] **Step 5: Commit.** `git add pkg/k8scount go.mod go.sum && git commit -m "feat(k8scount): cluster-mode NAD/MNP/NP/UDN collector"`

---

### Task 6: Exporter binary wiring (`cmd/ovnk-observ-exporter`)

**Files:**
- Modify: `cmd/ovnk-observ-exporter/main.go`
- Test: `cmd/ovnk-observ-exporter/main_test.go`

**Interfaces:**
- Consumes: `ovsdbmon.Run`, `ovsdbmon.NBTables/SBTables`, `metrics.NewNodeCollector`, `metrics.ParseNetworkLabelMode`, `metrics.DBState`, `k8scount.NewCollector`.
- Produces: flags `--mode` (`node`|`cluster`, required), `--listen` (default `:9410`), `--per-network-labels` (default `topN:50`), `--nb-socket` (default `/var/run/ovn/ovnnb_db.sock`), `--sb-socket` (default `/var/run/ovn/ovnsb_db.sock`). `func run(ctx context.Context, args []string, stdout io.Writer) error`. Registry is a fresh `prometheus.NewRegistry()` plus process and Go collectors. `/healthz` returns 200 once the HTTP server is up. SIGTERM cancels ctx.

- [ ] **Step 1: Write failing tests.**
  - `TestRunRejectsBadMode`: `--mode=foo` → error containing `mode`.
  - `TestRunRejectsBadNetworkLabels`: `--per-network-labels=top:5` → error.
  - `TestNodeModeServesMetricsWithoutSockets`: `--mode=node --listen=127.0.0.1:0 --nb-socket=/nonexistent ...` → GET `/metrics` contains `ovnk_observ_db_connected{db="nb"} 0`; `/healthz` 200. (Expose the bound address via a test hook.)
- [ ] **Step 2: Run** `go test ./cmd/... -v`. Expected: FAIL.
- [ ] **Step 3: Implement.** Node mode starts two `ovsdbmon.Run` goroutines (databases `OVN_Northbound`, `OVN_Southbound`). Cluster mode uses `rest.InClusterConfig()`.
- [ ] **Step 4: Run** `go test ./... -race`. Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -am "feat: exporter binary with node and cluster modes"`

---

### Task 7: Deploy manifests, image build, and live count verification

**Files:**
- Create: `Dockerfile`, `deploy/kustomization.yaml`, `deploy/namespace.yaml`, `deploy/rbac.yaml`, `deploy/build.yaml`, `deploy/node-daemonset.yaml`, `deploy/cluster-deployment.yaml`, `deploy/services.yaml`, `deploy/servicemonitors.yaml`, `hack/verify-counts.sh`
- Modify: `Makefile` (targets `image`, `deploy`, `undeploy`, `verify-counts`)

**Interfaces:**
- Consumes: `bin/ovnk-observ-exporter` from `make build`.
- Produces: ImageStream `ovnk-observ/ovnk-observ-exporter:latest`; Services `ovnk-observ-node`, `ovnk-observ-cluster` (port name `metrics`, 9410); ServiceMonitors relabel `__meta_kubernetes_pod_node_name` → `node`.

Decisions:
- `Dockerfile`: `FROM registry.access.redhat.com/ubi9/ubi-micro:latest`, `COPY bin/ovnk-observ-exporter /usr/bin/`, `USER 0` (node mode needs root for the socket), `ENTRYPOINT ["/usr/bin/ovnk-observ-exporter"]`.
- `deploy/build.yaml`: ImageStream + BuildConfig `source.type: Binary`, `strategy.dockerStrategy`, output to the ImageStreamTag. `make image` = `make build && oc start-build ovnk-observ-exporter -n ovnk-observ --from-dir=. --follow` (with a `.dockerignore` excluding everything but `bin/` and `Dockerfile`).
- Both workloads name their container `exporter` (dashboard and Task 7 Step 5 queries rely on it).
- Node DaemonSet: `--mode=node`; hostPath `/var/run/ovn` (type Directory) mounted at `/var/run/ovn` read-write (unix `connect` needs write on the socket); `securityContext.privileged: true` with SA `ovnk-observ-node` bound to SCC `privileged` via ClusterRole `use`; tolerations `- operator: Exists`; PriorityClass `ovnk-observ` value `1000000` (below the system classes); resources per Global Constraints; image from ImageStream via `image.openshift.io/triggers` annotation.
- Cluster Deployment: `--mode=cluster`, SA `ovnk-observ-cluster`, ClusterRole get/list/watch on `network-attachment-definitions`, `multi-networkpolicies` (`k8s.cni.cncf.io`), `userdefinednetworks`, `clusteruserdefinednetworks` (`k8s.ovn.org`), `networkpolicies` (`networking.k8s.io`), `namespaces`. Requests `20m`/`64Mi`, limits `200m`/`256Mi`.
- Prometheus access: Role + RoleBinding in `ovnk-observ` granting `get/list/watch` on `services,endpoints,pods` to SA `prometheus-k8s` in `openshift-monitoring`.
- `hack/verify-counts.sh NODE`: finds that node's ovnkube-node pod; gets ACL and Port_Group counts via `ovsdb-client transact` select `_uuid` (the method used in brainstorming); queries thanos for `sum by (table) (ovnkube_controller_nb_db_objects{node="NODE",table=~"ACL|Port_Group"})`; exits non-zero if they differ.

- [ ] **Step 1: Write manifests and Makefile targets.** Run `kustomize build deploy/ >/dev/null`. Expected: exit 0.
- [ ] **Step 2: Deploy.** Run `oc apply -k deploy/ && make image && oc rollout status ds/ovnk-observ-node -n ovnk-observ --timeout=10m && oc rollout status deploy/ovnk-observ-cluster -n ovnk-observ`. Expected: all rolled out, 5 node pods Running.
- [ ] **Step 3: Check sync logs.** Run `oc logs -n ovnk-observ -l app=ovnk-observ-node --tail=20 | grep -i "initial sync"`. Expected: one NB and one SB line per pod with duration.
- [ ] **Step 4: Verify counts.** Wait 2 scrape intervals, then `hack/verify-counts.sh <worker-node>`. Expected: `ACL match`, `Port_Group match` (lab baseline ≈ 464,102 / 474,791 — the script compares live values, not the baseline).
- [ ] **Step 5: Verify memory.** Query `max(container_memory_working_set_bytes{namespace="ovnk-observ",container="exporter"})`. Expected: < 268435456 (256 Mi). If not, stop and report — do not raise the limit.
- [ ] **Step 6: Verify cluster series.** Query `sum(ovnkube_clustermanager_multi_network_policies)` → 3150; `sum(ovnkube_clustermanager_multi_network_policy_network_targets)` → 3150 × 147 = 463050 (re-check with `oc` if objects changed).
- [ ] **Step 7: Commit.** `git add Dockerfile .dockerignore deploy hack/verify-counts.sh Makefile && git commit -m "feat(deploy): manifests, in-cluster build, count verification"`

---

### Task 8: Integration test against a real ovsdb-server

**Files:**
- Create: `pkg/ovsdbmon/integration_test.go` (build tag `integration`), `hack/integration/run.sh`

**Interfaces:**
- Consumes: `ovsdbmon.Run`, `nbcount.Counter`.
- Produces: `make integration`.

Decisions: `run.sh` compiles `go test -c -tags integration -o bin/ovsdbmon.test ./pkg/ovsdbmon` (linux/amd64), starts a throwaway pod in `ovnk-observ` using the image of the `nbdb` container from any ovnkube-node pod (it ships `ovsdb-server`, `ovsdb-tool`, `ovn-nbctl`, and `/usr/share/ovn/ovn-nb.ovsschema`), copies the test binary in with `oc cp`, runs it, deletes the pod. The test itself: `ovsdb-tool create` a DB in `t.TempDir()`, start `ovsdb-server --remote=punix:<sock>`, insert 1,000 ACLs (`k8s.ovn.org/owner-controller=net1-network-controller`, `owner-type=NetworkPolicy`) and 500 Port_Groups via `ovn-nbctl --db=unix:<sock>`, run `ovsdbmon.Run`, assert snapshot `ACL{NetworkPolicy,net1}=1000`, `Port_Group` total 500; then `ovn-nbctl acl-del` the switch's ACLs (or `destroy` 10 ACL rows) and assert 990 within 5s plus `Updates()[{ACL,delete}]==10`; kill `ovsdb-server` and assert `OnState(false,…)`.

- [ ] **Step 1: Write the test and script.**
- [ ] **Step 2: Run** `make integration`. Expected: `PASS` and pod deleted.
- [ ] **Step 3: Commit.** `git add pkg/ovsdbmon/integration_test.go hack/integration Makefile && git commit -m "test: ovsdbmon integration against real ovsdb-server"`

---

### Task 9: Recording rules and alerts

**Files:**
- Create: `rules/ovnk-observ-rules.yaml` (rule groups only, promtool format), `rules/tests/rules_test.yaml`, `deploy/prometheusrule.yaml` (generated)
- Modify: `Makefile` (`rules-test`: `bin/promtool check rules rules/ovnk-observ-rules.yaml && bin/promtool test rules rules/tests/rules_test.yaml`; `rules-gen`: wrap into a `PrometheusRule` named `ovnk-observ` with label `role: alert-rules`), `deploy/kustomization.yaml`

Let `NODE_JOIN` = `* on (namespace, pod) group_left(node) kube_pod_info`. Group `ovnk-observ.rules`, interval `30s`:

| Record | Expression |
|---|---|
| `ovnk:nb_db_objects:sum_by_node_table` | `sum by (node, table) (ovnkube_controller_nb_db_objects)` |
| `ovnk:nb_db_objects:max_by_table` | `max by (table) (ovnk:nb_db_objects:sum_by_node_table)` |
| `ovnk:nb_db_objects:max_by_network_table` | `max by (network, table) (sum by (node, network, table) (ovnkube_controller_nb_db_objects))` |
| `ovnk:portgroup_amplification:ratio` | `max(sum by (node) (ovnkube_controller_nb_db_objects{table="Port_Group",owner_type="NetworkPolicy"})) / clamp_min(sum(ovnkube_clustermanager_multi_network_policies) + sum(ovnkube_clustermanager_network_policies), 1)` |
| `ovnk:nb_sb_effectiveness:ratio` | `sum by (node) (ovnkube_controller_sb_db_objects{table="Logical_Flow"}) / clamp_min(sum by (node) (ovnkube_controller_nb_db_objects{table="ACL"}), 1)` |
| `ovnk:network_programming:p99_5m` | `histogram_quantile(0.99, sum by (le) (rate(ovnkube_controller_network_programming_duration_seconds_bucket[5m])))` |
| `ovnk:pod_setup_stage:p99_5m` (4 rules, static label `stage`) | `histogram_quantile(0.99, sum by (le) (rate(<metric>_bucket[5m])))` with `first_seen_lsp`→`ovnkube_controller_pod_first_seen_lsp_created_duration_seconds`, `lsp_port_binding`→`..._pod_lsp_created_port_binding_duration_seconds`, `port_binding_chassis`→`..._pod_port_binding_port_binding_chassis_duration_seconds`, `chassis_up`→`..._pod_port_binding_chassis_port_binding_up_duration_seconds` |
| `ovnk:ovn_txn_failure:ratio_5m` (2 rules, static label `component` = `northd`/`controller`) | `(sum by (namespace, pod) (rate(P_txn_error[5m]) + rate(P_txn_try_again[5m]) + rate(P_txn_aborted[5m])) / clamp_min(sum by (namespace, pod) (rate(P_txn_success[5m]) + rate(P_txn_error[5m]) + rate(P_txn_try_again[5m]) + rate(P_txn_aborted[5m])), 1e-9)) NODE_JOIN` with `P` = `ovn_northd` / `ovn_controller` |
| `ovnk:nb_sb_e2e_lag_seconds` | `(ovnkube_controller_nb_e2e_timestamp - ovnkube_controller_sb_e2e_timestamp) NODE_JOIN` |
| `ovnk:e2e_probe_staleness_seconds` | `(time() - ovnkube_controller_nb_e2e_timestamp) NODE_JOIN` |
| `ovnk:ovn_db_size_bytes:deriv_30m` | `deriv(ovn_db_db_size_bytes[30m]) NODE_JOIN` |

Alerts (`severity: warning`, `for: 15m` unless noted; summary names the node/value):

| Alert | Expr |
|---|---|
| `OVNKPortGroupAmplificationHigh` | `ovnk:portgroup_amplification:ratio > 10` |
| `OVNKNBDBObjectsHigh` | `ovnk:nb_db_objects:sum_by_node_table{table=~"ACL\|Port_Group"} > 100000` |
| `OVNKNBDBGrowing` | `ovnk:ovn_db_size_bytes:deriv_30m{db_name="OVN_Northbound"} * 3600 > 50e6`, `for: 1h` |
| `OVNKPropagationLagHigh` | `ovnk:nb_sb_e2e_lag_seconds > 30` |
| `OVNKE2EProbeStale` | `ovnk:e2e_probe_staleness_seconds > 180` |
| `OVNKTxnFailureRatioHigh` | `ovnk:ovn_txn_failure:ratio_5m > 0.05` |
| `OVNKNetworkProgrammingSlow` | `ovnk:network_programming:p99_5m > 10` |

- [ ] **Step 1: Write failing promtool tests** in `rules/tests/rules_test.yaml`:
  - amplification: node A PortGroups(NetworkPolicy)=474791, MNPs=3150, NPs=157 → ratio `143.57…` (assert `474791/3307`), alert `OVNKPortGroupAmplificationHigh` firing at 16m.
  - effectiveness: Logical_Flow=1562, ACL=464102 on node A → `1562/464102`.
  - txn failure: northd success +100/min, try_again +10/min, kube_pod_info maps pod→node A → ratio `10/110` with `node="A"`, `component="northd"`.
  - lag: nb_e2e=1000, sb_e2e=950 → lag 50, alert `OVNKPropagationLagHigh` firing at 16m.
  - growing: NB size rising 1e6 bytes/min for 2h → `OVNKNBDBGrowing` firing at 75m, not at 30m.
  - no-fire: amplification 2 → no alert.
- [ ] **Step 2: Run** `make rules-test`. Expected: FAIL (rules missing).
- [ ] **Step 3: Write `rules/ovnk-observ-rules.yaml`** per the tables.
- [ ] **Step 4: Run** `make rules-test`. Expected: `SUCCESS`.
- [ ] **Step 5: Apply and check live.** `make rules-gen && oc apply -k deploy/`; after 2 minutes query `ovnk:portgroup_amplification:ratio`. Expected: one sample ≈ 143 on the lab; no rule-evaluation errors in `ALERTS`/`prometheus_rule_evaluation_failures_total{rule_group=~".*ovnk-observ.*"}` = 0.
- [ ] **Step 6: Commit.** `git add rules deploy Makefile && git commit -m "feat(rules): recording rules, alerts, promtool tests"`

---

### Task 10: Dashboard generator, ConfigMap, and live panel verification

**Files:**
- Create: `hack/dashgen/main.go`, `hack/dashgen/panels.go`, `hack/dashgen/panels_test.go`, `dashboards/ovn-scale-troubleshooting.json` (generated), `deploy/dashboard-configmap.yaml` (generated), `hack/verify-panels.sh`
- Modify: `Makefile` — `dashboard`: `go run ./hack/dashgen -out dashboards/ -configmap deploy/dashboard-configmap.yaml`; `dashboard-apply`: `oc apply -f deploy/dashboard-configmap.yaml`; `undeploy` also deletes it. The ConfigMap lives in `openshift-config-managed`, so it stays out of the kustomize tree.

**Interfaces:**
- Produces: `func Build() Dashboard` (Grafana schema used by the console: `rows[]` each with `panels[]`, `templating.list[]`, `links[]`); `type Panel struct{ ID int; Title, Type string; Span int; Targets []Target; Format string; Thresholds string; ... }`; `type Target struct{ Expr, LegendFormat string }`.

Variables: `datasource` (type `datasource`, `prometheus`); `node` (query `label_values(ovnkube_controller_nb_db_objects, node)`, includeAll, allValue `.*`); `network` (query `label_values(ovnkube_controller_nb_db_objects, network)`, includeAll, allValue `.*`); `interval` (`1m,5m,15m`, default `5m`). Link: dashboard `Networking / Infrastructure`.

Rows and panels (`N` = `{node=~"$node"}` selector; `J` = NODE_JOIN filtered by `node=~"$node"`):

| Row | Panel (type) | Expr |
|---|---|---|
| At a glance | Max NB ACLs on a node (singlestat) | `ovnk:nb_db_objects:max_by_table{table="ACL"}` |
| | Max NB PortGroups on a node (singlestat) | `ovnk:nb_db_objects:max_by_table{table="Port_Group"}` |
| | Largest NB DB (singlestat, bytes) | `max(ovn_db_db_size_bytes{db_name="OVN_Northbound"})` |
| | PortGroup amplification (gauge, thresholds 5/10) | `ovnk:portgroup_amplification:ratio` |
| | Network programming p99 (singlestat, s, thresholds 2/10) | `ovnk:network_programming:p99_5m` |
| | Retry failures 15m (singlestat) | `sum(increase(ovnkube_resource_retry_failures_total[15m]))` |
| | Nodes disconnected (singlestat, thresholds 1/1) | `count(ovn_northd_nb_connection_status == 0 or ovn_northd_sb_connection_status == 0 or ovn_controller_southbound_database_connected == 0) or vector(0)` |
| Scale & inventory | Kubernetes network objects (graph) | `sum by (managed_by) (ovnkube_clustermanager_network_attachment_definitions)`, `sum(ovnkube_clustermanager_multi_network_policies)`, `sum(ovnkube_clustermanager_multi_network_policy_network_targets)`, `sum(ovnkube_clustermanager_network_policies)`, `sum by (kind) (ovnkube_clustermanager_user_defined_networks)` |
| | NB objects per node by table (graph) | `ovnk:nb_db_objects:sum_by_node_table{node=~"$node"}` legend `{{node}} {{table}}` |
| | Top networks by ACL / PortGroup (table, instant) | `topk(15, ovnk:nb_db_objects:max_by_network_table{table=~"ACL\|Port_Group",network=~"$network"})` |
| | NB ACL vs SB Logical_Flow (graph) | `ovnk:nb_db_objects:sum_by_node_table{table="ACL",node=~"$node"}`, `sum by (node) (ovnkube_controller_sb_db_objects{table="Logical_Flow",node=~"$node"})` |
| | ACLs by owner type (graph) | `sum by (owner_type) (ovnkube_controller_nb_db_objects{table="ACL",node=~"$node",network=~"$network"})` |
| Programming latency & backlog | Network programming p50/p99 (graph) | `histogram_quantile(0.5\|0.99, sum by (le) (rate(ovnkube_controller_network_programming_duration_seconds_bucket[$interval])))` and the `_ovn_` variant p99 |
| | Resource add/update/delete p99 (graph) | `histogram_quantile(0.99, sum by (le, kind) (rate(ovnkube_controller_resource_{add,update,delete}_latency_seconds_bucket[$interval])))` — 3 targets; confirm the kind-like label name with `label_names` query first and use it |
| | Pod setup pipeline p99 (graph, stacked) | `ovnk:pod_setup_stage:p99_5m` legend `{{stage}}` |
| | Retry failures by node (graph) | `sum by (node) (rate(ovnkube_resource_retry_failures_total[$interval]) J)` |
| | NB→SB lag / probe staleness (graph) | `ovnk:nb_sb_e2e_lag_seconds{node=~"$node"}`, `ovnk:e2e_probe_staleness_seconds{node=~"$node"}` |
| NB/SB DB health | DB size (graph, bytes) | `ovn_db_db_size_bytes J` legend `{{node}} {{db_name}}` |
| | DB growth per hour (graph, bytes) | `ovnk:ovn_db_size_bytes:deriv_30m{node=~"$node"} * 3600` |
| | nbdb/sbdb CPU (graph) | `sum by (node, container) (rate(container_cpu_usage_seconds_total{namespace="openshift-ovn-kubernetes",container=~"nbdb\|sbdb",node=~"$node"}[$interval]))` |
| | nbdb/sbdb RSS (graph, bytes) | `sum by (node, container) (container_memory_rss{namespace="openshift-ovn-kubernetes",container=~"nbdb\|sbdb",node=~"$node"})` |
| | Sessions & monitors (graph) | `ovn_db_jsonrpc_server_sessions J`, `ovn_db_ovsdb_monitors J` |
| | Connection status (table, instant) | `ovn_northd_nb_connection_status J`, `ovn_northd_sb_connection_status J`, `ovn_controller_southbound_database_connected J` |
| | libovsdb disconnects (graph) | `sum by (node) (rate(ovnkube_master_libovsdb_disconnects_total[$interval]) J)` |
| Transactions & churn | NB update rate by table/op (graph) | `sum by (table, op) (rate(ovnkube_controller_nb_db_updates_total{node=~"$node"}[$interval]))` |
| | northd txn rate by result (graph) | 4 targets `sum(rate(ovn_northd_txn_{success,try_again,error,aborted}[$interval]) J)` |
| | ovn-controller txn rate by result (graph) | same with `ovn_controller_txn_*` |
| | Txn failure ratio (graph, percentunit) | `ovnk:ovn_txn_failure:ratio_5m{node=~"$node"}` legend `{{node}} {{component}}` |
| Recompute cost | northd loop p95 / max (graph, ms) | `ovn_northd_ovn_northd_loop_95th_percentile J`, `ovn_northd_ovn_northd_loop_maximum J` |
| | northd build_lflows / nb_db_run / sb_db_run p95 (graph, ms) | `ovn_northd_build_lflows_95th_percentile J`, `ovn_northd_ovnnb_db_run_95th_percentile J`, `ovn_northd_ovnsb_db_run_95th_percentile J` |
| | ovn-controller lflow_run rate (graph) | `rate(ovn_controller_lflow_run[$interval]) J` |
| | Flow generation / installation p95 (graph, ms) | `ovn_controller_flow_generation_95th_percentile J`, `ovn_controller_flow_installation_95th_percentile J` |
| | br-int OpenFlow count (graph) | `ovn_controller_integration_bridge_openflow_total J` |
| Exporter self-cost (collapsed) | Exporter CPU / RSS (graph) | `sum by (node) (rate(container_cpu_usage_seconds_total{namespace="ovnk-observ"}[$interval]))`, `sum by (node) (container_memory_working_set_bytes{namespace="ovnk-observ",container="exporter"})` |
| | DB connected / initial sync (table, instant) | `ovnk_observ_db_connected`, `ovnk_observ_initial_sync_seconds` |

- [ ] **Step 1: Write failing tests** in `panels_test.go`:
  - `TestDashboardRows`: row titles equal, in order, `At a glance`, `Scale & inventory`, `Programming latency & backlog`, `NB/SB DB health`, `Transactions & churn`, `Recompute cost`, `Exporter self-cost`; last row `collapse: true`.
  - `TestPanelsValid`: every panel has unique ID, type in the allowed set, ≥1 target, no empty `Expr`, no literal `NODE_JOIN`/`J` placeholder left.
  - `TestTitleAndVariables`: title from Global Constraints; variables `datasource,node,network,interval`.
  - `TestConfigMapWraps`: generated ConfigMap has the name, namespace, label, and data key from Global Constraints, and its JSON equals `Build()` output.
- [ ] **Step 2: Run** `go test ./hack/dashgen/ -v`. Expected: FAIL.
- [ ] **Step 3: Implement** `panels.go` per the table (a helper `join(expr)` appends the node join + `node=~"$node"` filter) and `main.go` writer.
- [ ] **Step 4: Run** `go test ./hack/dashgen/ -v && make dashboard`. Expected: PASS; files regenerated.
- [ ] **Step 5: Write `hack/verify-panels.sh`.** Extracts every `expr` from the JSON with `jq`, substitutes `$node`/`$network`→`.*`, `$interval`→`5m`, runs `/api/v1/query` on thanos-querier, prints `OK|EMPTY|ERROR title`; exits 1 on any `ERROR` or on `EMPTY` for panels outside the `Exporter self-cost` row.
- [ ] **Step 6: Run** `make dashboard-apply && hack/verify-panels.sh`. Expected: all `OK`. Fix queries (not the script) on failure.
- [ ] **Step 7: Render check.** Open `https://<console-host>/monitoring/dashboards/grafana-dashboard-ovn-scale-troubleshooting` in the browser (claude-in-chrome), screenshot rows 0–1. Expected: dashboard in the dropdown, no "unsupported panel" placeholders, amplification gauge red ≈ 143.
- [ ] **Step 8: Commit.** `git add hack/dashgen hack/verify-panels.sh dashboards deploy/dashboard-configmap.yaml Makefile && git commit -m "feat(dashboard): generator, ConfigMap, live panel verification"`

---

### Task 11: Scale-delta check (gated) and README

**Files:**
- Create: `README.md` (what it is, `make deploy image rules-gen dashboard-apply`, `make undeploy`, metric catalog link to spec, upstream path)

- [ ] **Step 1: Ask the user** for approval to delete MNPs in `<repro-namespace>`. If declined, skip to Step 4 and record "skipped" in the README's verification notes.
- [ ] **Step 2: Delete 10 MNPs**, wait 2 minutes, query `ovnk:nb_db_objects:max_by_table{table="Port_Group"}` before/after. Expected: drop ≈ 10 × 147 × (PortGroups per MNP-network; derive from the before value ÷ 463,050 targets) within 10%.
- [ ] **Step 3: Restore** the deleted MNPs from the YAML saved before deletion (`oc get mnp ... -o yaml > $SCRATCH/mnp-backup.yaml`) and confirm count returns to 3150.
- [ ] **Step 4: Write README; verify** `make undeploy` removes namespace, ClusterRoles, PriorityClass, and the dashboard ConfigMap (`oc get cm -n openshift-config-managed grafana-dashboard-ovn-scale-troubleshooting` → NotFound), then redeploy with `make deploy image rules-gen dashboard-apply`.
- [ ] **Step 5: Commit.** `git add README.md && git commit -m "docs: README and scale-delta verification notes"`
