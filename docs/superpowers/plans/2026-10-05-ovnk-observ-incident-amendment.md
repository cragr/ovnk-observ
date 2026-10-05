# OVN-K Observ Incident Amendment Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add NB↔SB Port_Group drift, northd incremental-processing stats, policy churn and fan-out metrics, then restructure the dashboard and cut its query load.

**Architecture:** Node mode gains a `pkg/pgdrift` tracker fed by a row hook on the existing streaming OVSDB monitors, and a `pkg/incengine` collector that calls `inc-engine/show-stats` through a single-command `pkg/appctl` client. Cluster mode gains informer event handlers and richer MNP transforms in `pkg/k8scount`. Rules, dashboard (`hack/dashgen`) and `hack/verify-panels.sh` are updated to match, and generated artifacts are re-rendered with the existing make targets.

**Tech Stack:** Go 1.26, client-go v0.36.2, prometheus/client_golang, promtool, kustomize v5.8.1, bash + jq, OpenShift `oc`.

**Spec:** `docs/superpowers/specs/2026-10-05-ovnk-observ-incident-amendment-design.md` (amends `docs/superpowers/specs/2026-10-02-ovnk-observ-dashboard-design.md`).

## Global Constraints

- No new Go module dependencies. Hashing uses `hash/fnv` (64-bit FNV-1a).
- OVSDB access stays monitor v1 (`"monitor"`) only; the exporter never sends `transact`.
- The only appctl command the exporter can send is the unexported constant `"inc-engine/show-stats"`. No exported function takes a command string.
- Node DaemonSet resources stay at requests 50m/64Mi, limits 500m/256Mi.
- Metric names and labels exactly as spec §5: `ovnkube_controller_port_group_sb_missing`, `ovnkube_controller_port_group_with_ports`, `ovn_northd_inc_engine_runs_total{engine_node,type}`, `ovnkube_clustermanager_object_events_total{resource,op,manager}`, `ovnkube_clustermanager_multi_network_policy_rules{namespace,direction}`, `ovnkube_clustermanager_multi_network_policy_peers{namespace,direction}`, `ovnk_observ_appctl_errors_total{command}`.
- `resource` label values reuse the existing set: `nad`, `mnp`, `networkpolicy`, `udn`, `cudn`. `op` is `add`/`update`/`delete`. `direction` is `ingress`/`egress`. `type` is `recompute`/`compute`/`cancel`.
- Default inc-engine allowlist: `northd,lflow,port_group,sync_to_sb_addr_set,sync_from_sb,ls_stateful,lr_stateful`.
- Flags and defaults: `--pg-drift=true`, `--inc-engine=true`, `--inc-engine-nodes=<allowlist>`, `--churn-manager-label=topN:10`.
- Generated files are never hand-edited: `deploy/prometheusrule.yaml` (`make rules-gen`), `dashboards/` and `deploy/dashboard-configmap.yaml` (`make dashboard`), `install/ovnk-observ.yaml` (`make install-manifest`).
- Console dashboard constraints from the base spec still hold: classic rows layout, panel types graph/singlestat/table only, singlestat colors via `options.fieldOptions.thresholds`, `light-red` for critical, each row's spans sum to a multiple of 12.
- Lab testing uses whatever cluster `oc` is logged into at test time. Run `oc whoami --show-server` first and report it. Ask the user before `inc-engine/recompute`, before changing the repro namespace, and before `make image-push`.
- No customer names, hosts, IPs, or policy names in code, fixtures, test data, or commit messages.

## Review Focus

1. **SB reconnects while NB stays up.** SB rows reset to empty, so every NB Port_Group with ports would look missing. Expect: no drift series until SB has re-synced. Test: Task 4 `TestDriftCollectorSuppressedUntilBothSynced`.
2. **northd restarts and gets a new PID.** A cached `.ctl` path goes stale. Expect: the next scrape finds the new socket. Test: Task 5 `TestShowStatsFollowsPIDChange`.
3. **Port_Group membership flips.** Ports go from non-empty to empty and back, or a row is renamed. Expect: `withPorts` and `missing` follow each change with no double counting. Test: Task 2 `TestTrackerPortsTransitions` and `TestTrackerRename`.
4. **Informer relist after a watch expires.** Replace emits updates with an unchanged `resourceVersion`. Expect: those are not counted as churn. Test: Task 8 `TestChurnSkipsUnchangedResourceVersion`.
5. **Unexpected `show-stats` output or an error reply.** An older OVN might reject the command or format output differently. Expect: unknown lines are ignored, an error reply increments `ovnk_observ_appctl_errors_total`, and nothing panics. Tests: Task 6 `TestParseToleratesNoise` and Task 5 `TestShowStatsErrorReply`.

---

### Task 1: Lab precheck and parser fixtures (read-only)

Confirms the facts the node-mode features depend on and captures a `show-stats` fixture. Reads the cluster only.

**Files:**
- Create: `pkg/incengine/testdata/show-stats-lab.txt`
- Create: `pkg/incengine/testdata/show-stats-incident.txt`
- Modify: `docs/superpowers/specs/2026-10-05-ovnk-observ-incident-amendment-design.md` (only if a fact below differs)

- [ ] **Step 1: Identify the cluster.** Run `oc whoami --show-server` and `oc get clusterversion version -o jsonpath='{.status.desired.version}'`. Report both to the user before continuing.
- [ ] **Step 2: Check the northd run directory.** Pick one node `N` from `oc get pods -n openshift-ovn-kubernetes -l app=ovnkube-node -o wide`. Run `oc debug node/N -q -- chroot /host ls /var/run/ovn-ic`. Expected: `ovnnb_db.sock`, `ovnsb_db.sock`, `ovn-northd.pid`, `ovn-northd.<pid>.ctl`. If the pid file or ctl socket lives elsewhere, record the path in spec §4 and use it in Task 5 instead of `filepath.Dir(--nb-socket)`.
- [ ] **Step 3: Capture show-stats.** Run `oc exec -n openshift-ovn-kubernetes <ovnkube-node pod on N> -c northd -- ovn-appctl -t ovn-northd inc-engine/show-stats > pkg/incengine/testdata/show-stats-lab.txt`. The output contains only engine node names and counters.
- [ ] **Step 4: Confirm the SB name format.** Run `oc exec … -c sbdb -- ovn-sbctl --no-leader-only --bare --columns=name list Port_Group | head -3`. Expected: names match `^[0-9]+_`. Do not save the names anywhere.
- [ ] **Step 5: Write the incident fixture.** Copy the `inc-engine/show-stats` block from the incident (the `Node: NB_acl` … `Node: northd_output` lines, counters only) into `pkg/incengine/testdata/show-stats-incident.txt`.
- [ ] **Step 6: Commit**

```bash
git add pkg/incengine/testdata
git commit -m "test(incengine): show-stats fixtures from lab and incident"
```

---

### Task 2: `pkg/pgdrift` tracker

**Files:**
- Create: `pkg/pgdrift/tracker.go`
- Test: `pkg/pgdrift/tracker_test.go`

**Interfaces:**
- Produces:
  - `func NewTracker() *Tracker`
  - `func (t *Tracker) NB() *Side`, `func (t *Tracker) SB() *Side`
  - `func (s *Side) Upsert(table string, uuid [16]byte, row map[string]json.RawMessage)`, `func (s *Side) Delete(table string, uuid [16]byte)`, `func (s *Side) Reset()`. These satisfy `ovsdbmon.RowHook` from Task 3. Rows of any table other than `Port_Group` are ignored.
  - `func (t *Tracker) Missing() (missing, withPorts int)`
  - `func HasPorts(raw json.RawMessage) bool`
  - `func SBBaseName(name string) string`: strips a leading `^[0-9]+_`; returns the input unchanged otherwise.

- [ ] **Step 1: Write the failing tests**

```go
func TestHasPorts(t *testing.T) {
	for raw, want := range map[string]bool{
		`["set",[]]`: false, `["set", [ ]]`: false, ``: false,
		`["uuid","0b8d6c3e-7f1a-4c3e-9d2a-1a2b3c4d5e6f"]`: true,
		`["set",[["uuid","0b8d6c3e-7f1a-4c3e-9d2a-1a2b3c4d5e6f"]]]`: true,
	} { /* assert HasPorts(json.RawMessage(raw)) == want */ }
}
func TestSBBaseName(t *testing.T) {
	// "12_pgA" -> "pgA"; "pgA" -> "pgA"; "12pgA" -> "12pgA"; "_pgA" -> "_pgA"
}
func TestTrackerMissing(t *testing.T) {
	// NB: pgA(ports), pgB(ports), pgC(no ports). SB: "3_pgA", "7_pgA".
	// Missing() == (1, 2)   // pgB missing; pgC not counted
	// Delete SB "3_pgA" row -> still (1, 2) (refcount); delete "7_pgA" -> (2, 2)
}
func TestTrackerPortsTransitions(t *testing.T) {
	// pgB with ports, no SB -> (1,1); Upsert pgB with ["set",[]] -> (0,0); back to ports -> (1,1)
}
func TestTrackerRename(t *testing.T) {
	// NB uuid U named pgA with ports, SB has "1_pgA" -> (0,1); Upsert U named pgZ -> (1,1)
}
func TestTrackerResetSide(t *testing.T) {
	// NB pgA(ports) + SB "1_pgA" -> (0,1); SB().Reset() -> (1,1); NB().Reset() -> (0,0)
}
func TestTrackerIgnoresOtherTables(t *testing.T) {
	// NB().Upsert("ACL", ...) changes nothing
}
```

- [ ] **Step 2: Run the tests.** Run `go test ./pkg/pgdrift/`. Expected: FAIL (package missing).
- [ ] **Step 3: Implement `tracker.go`.** One `sync.Mutex` guards three maps: NB `map[[16]byte]nbRow{hash uint64; hasPorts bool}`, SB `map[[16]byte]uint64`, and SB refcounts `map[uint64]uint32` (delete the key when it reaches 0). The hash is FNV-1a 64 of the decoded `name` string; SB rows hash `SBBaseName(name)`. On a modify, an SB Upsert of a known uuid first decrements the old hash. `HasPorts` scans bytes without unmarshalling large sets: a `"uuid"` tag means true; for a `"set"` tag, true if the first non-space byte after the inner `[` is not `]`. `Missing()` makes one pass over NB.
- [ ] **Step 4: Run the tests.** Run `go test ./pkg/pgdrift/`. Expected: PASS.
- [ ] **Step 5: Commit**

```bash
git add pkg/pgdrift
git commit -m "feat(pgdrift): track NB Port_Groups with ports missing from SB"
```

---

### Task 3: `ovsdbmon` extra columns and row hook

**Files:**
- Modify: `pkg/ovsdbmon/client.go` (TableSpec, Config, `monitorRequest`, `Run`)
- Modify: `pkg/ovsdbmon/decode.go` (`session`, `apply`)
- Test: `pkg/ovsdbmon/client_test.go`

**Interfaces:**
- Produces:
  - `TableSpec.Extra []string`: additional monitored columns, requested after `Column`.
  - `type RowHook interface { Upsert(table string, uuid [16]byte, row map[string]json.RawMessage); Delete(table string, uuid [16]byte); Reset() }`
  - `Config.Hook RowHook` (optional). Called on every applied row: Upsert for `new`; Delete for `old`-only, non-initial. `Reset()` is called right after `Counter.Reset()` on every disconnect.
  - `func WithColumns(tables []TableSpec, table string, cols ...string) []TableSpec`: returns a copy. If `table` is present, `cols` are appended to its `Extra`. Otherwise a new `TableSpec{Name: table, Column: cols[0], Extra: cols[1:]}` is appended.

- [ ] **Step 1: Write the failing tests** in `client_test.go`:
  - `TestMonitorRequestIncludesExtraColumns`: the `Port_Group` columns equal `["external_ids","name","ports"]` for `WithColumns(NBTables, "Port_Group", "name", "ports")`.
  - `TestWithColumnsAddsMissingTable`: `WithColumns(SBTables, "Port_Group", "name")` ends with `{Name:"Port_Group", Column:"name"}`, and `SBTables` itself is unchanged.
  - `TestHookSeesRowsAndResets`: use the existing fake-server pattern. Send an initial dump with one Port_Group row, then an update deleting it, then close. Assert the hook saw Upsert(row containing `name`), Delete, then Reset.
- [ ] **Step 2: Run the tests.** Run `go test ./pkg/ovsdbmon/ -run 'Extra|WithColumns|Hook'`. Expected: FAIL.
- [ ] **Step 3: Implement** the fields, the helper, and the hook calls described above.
- [ ] **Step 4: Run all package tests.** Run `go test ./pkg/ovsdbmon/`. Expected: PASS.
- [ ] **Step 5: Commit**

```bash
git add pkg/ovsdbmon
git commit -m "feat(ovsdbmon): extra monitored columns and a row hook"
```

---

### Task 4: Drift collector, `--pg-drift`, integration test

**Files:**
- Create: `pkg/metrics/driftcollector.go`
- Test: `pkg/metrics/driftcollector_test.go`
- Modify: `cmd/ovnk-observ-exporter/main.go`
- Modify: `cmd/ovnk-observ-exporter/main_test.go`
- Modify: `pkg/ovsdbmon/integration_test.go`
- Modify: `hack/integration/run.sh`

**Interfaces:**
- Consumes: `pgdrift.Tracker` (Task 2); `ovsdbmon.WithColumns`, `Config.Hook` (Task 3); existing `metrics.DBState`.
- Produces: `type DriftSource interface{ Missing() (missing, withPorts int) }` and `func NewDriftCollector(src DriftSource, nbState, sbState *DBState) prometheus.Collector`.

- [ ] **Step 1: Write the failing tests**
  - `TestDriftCollectorEmits`: with both states `Set(true, …)` and a source returning `(3, 40)`, the output equals:

    ```
    ovnkube_controller_port_group_sb_missing 3
    ovnkube_controller_port_group_with_ports 40
    ```

  - `TestDriftCollectorSuppressedUntilBothSynced`: with NB connected and SB `Set(false, 0)`, there is no output. The same holds with the states swapped.
  - In `main_test.go`, `TestNodeModePGDriftFlag`: `--pg-drift=false` parses and node mode serves `/metrics` with no `port_group_sb_missing` series (sockets absent, as in `TestNodeModeServesMetricsWithoutSockets`).
- [ ] **Step 2: Run the tests.** Run `go test ./pkg/metrics/ -run Drift`. Expected: FAIL.
- [ ] **Step 3: Implement the collector.** In `main.go`, add the `--pg-drift` bool flag (default true). When it is true, start NB with `WithColumns(ovsdbmon.NBTables, "Port_Group", "name", "ports")` and `Hook: tr.NB()`. Start SB with `WithColumns(ovsdbmon.SBTables, "Port_Group", "name")` and `Hook: tr.SB()`. Register `NewDriftCollector(tr, nbState, sbState)`. When it is false, use the tables unchanged with no hook and no collector. Add `pg_drift` to the startup log line.
- [ ] **Step 4: Run the tests.** Run `go test ./...`. Expected: PASS.
- [ ] **Step 5: Extend the integration test.** Add `TestPGDriftAgainstRealOVSDBServer` in `integration_test.go`:
  - Start a second `ovsdb-server` on `/usr/share/ovn/ovn-sb.ovsschema`.
  - In NB, create two Port_Groups `pgA` and `pgB`, each holding one Logical_Switch_Port (`ovn-nbctl ls-add`, `lsp-add`, `pg-add pgA <lsp>`).
  - In SB, run `ovn-sbctl create Port_Group name=1_pgA` and `name=1_pgB`.
  - Run both monitors with the hooks. Wait until `Missing()` returns `(0, 2)`.
  - Destroy the SB row `1_pgB`. Wait for `(1, 2)`. Recreate it. Wait for `(0, 2)`.

  Change the `-test.run` and PASS checks in `hack/integration/run.sh` to cover both tests (`-test.run 'TestMonitorAgainstRealOVSDBServer|TestPGDriftAgainstRealOVSDBServer'`, and one `grep -q` per test).
- [ ] **Step 6: Run the integration test.** Run `oc whoami --show-server`, then `make integration`. Expected: both tests report `--- PASS`.
- [ ] **Step 7: Commit**

```bash
git add pkg/metrics cmd hack/integration pkg/ovsdbmon/integration_test.go
git commit -m "feat(node): export NB/SB Port_Group drift behind --pg-drift"
```

---

### Task 5: `pkg/appctl` single-command client

**Files:**
- Create: `pkg/appctl/appctl.go`
- Test: `pkg/appctl/appctl_test.go`

**Interfaces:**
- Produces:
  - `func FindCtl(runDir, target string) (string, error)`: reads `<runDir>/<target>.pid`, trims whitespace, and returns `<runDir>/<target>.<pid>.ctl`. Errors if the pid file is missing or not an integer.
  - `func ShowIncEngineStats(ctx context.Context, runDir string) (string, error)`: calls `FindCtl(runDir, "ovn-northd")` on every call (no caching), dials the unix socket, and sends `{"id":0,"method":"inc-engine/show-stats","params":[]}`. It decodes one reply, returns `result` as a string, and returns an error if `error` is non-null. It reads at most 1 MiB and honors the ctx deadline. The method string is the unexported `const showStatsCmd`.
  - `const ShowStatsCommand = "inc-engine/show-stats"`: exported only as the value of the `command` label.

- [ ] **Step 1: Write the failing tests.** Each uses a temp dir, a fake unix-socket server goroutine, and a pid file.
  - `TestShowStatsSendsOnlyShowStats`: the server captures the request. Assert `method == "inc-engine/show-stats"` and `params == []`. Return `{"id":0,"result":"Node: northd\n- recompute: 1\n","error":null}`. Assert the returned string.
  - `TestShowStatsErrorReply`: the server returns `{"id":0,"result":null,"error":"\"inc-engine/show-stats\" is not a valid command"}`. Assert a non-nil error containing `not a valid command`.
  - `TestShowStatsFollowsPIDChange`: pid file `100` with a server on `ovn-northd.100.ctl`. Call once. Then rewrite the pid file to `200`, start a server on `ovn-northd.200.ctl`, and close the first. The second call succeeds against the new server.
  - `TestShowStatsTimeout`: the server accepts and never replies. With a 100 ms ctx, the call returns `context.DeadlineExceeded` (use `errors.Is`).
  - `TestFindCtlBadPID`: a pid file containing `abc` returns an error.
- [ ] **Step 2: Run the tests.** Run `go test ./pkg/appctl/`. Expected: FAIL.
- [ ] **Step 3: Implement `appctl.go`.**
- [ ] **Step 4: Run the tests.** Run `go test ./pkg/appctl/`. Expected: PASS.
- [ ] **Step 5: Commit**

```bash
git add pkg/appctl
git commit -m "feat(appctl): read-only inc-engine/show-stats client for ovn-northd"
```

---

### Task 6: `pkg/incengine` parser and collector, wiring

**Files:**
- Create: `pkg/incengine/incengine.go`
- Test: `pkg/incengine/incengine_test.go` (uses the Task 1 fixtures)
- Modify: `cmd/ovnk-observ-exporter/main.go`, `cmd/ovnk-observ-exporter/main_test.go`

**Interfaces:**
- Consumes: `appctl.ShowIncEngineStats`, `appctl.ShowStatsCommand` (Task 5).
- Produces:
  - `type Stats struct{ Recompute, Compute, Cancel uint64 }`
  - `func Parse(s string) map[string]Stats`
  - `var DefaultNodes = []string{"northd","lflow","port_group","sync_to_sb_addr_set","sync_from_sb","ls_stateful","lr_stateful"}`
  - `func NewCollector(fetch func(context.Context) (string, error), nodes []string, timeout time.Duration) prometheus.Collector`: emits `ovn_northd_inc_engine_runs_total{engine_node,type}` (counter) for allowlisted nodes that are present, and `ovnk_observ_appctl_errors_total{command}` (counter, always emitted, starting at 0).

- [ ] **Step 1: Write the failing tests**
  - `TestParseIncidentFixture`: `Parse(fixture)["northd"] == Stats{167, 716840, 0}`, `["lflow"] == Stats{13320, 402740, 0}`, `["NB_acl"].Compute == 0`.
  - `TestParseLabFixture`: the result has a `northd` key, and every `DefaultNodes` entry present in the fixture parses with `Recompute+Compute > 0`.
  - `TestParseToleratesNoise`: input with blank lines, `\r\n`, a `Node: x` header with no stat lines, an unknown `- foo: 3` line, and a non-numeric value parses without panic. `x` is present with zero `Stats`; `foo` is ignored.
  - `TestCollectorAllowlist`: a fetch returning the incident fixture with nodes `["northd","not_a_node"]` produces exactly 3 `runs_total` series, all with `engine_node="northd"`, plus the errors counter at 0.
  - `TestCollectorCachesOneScrape`: fetch succeeds, then fails twice. Scrape 2 still shows the scrape-1 values with errors=1. Scrape 3 shows no `runs_total` series and errors=2.
- [ ] **Step 2: Run the tests.** Run `go test ./pkg/incengine/`. Expected: FAIL.
- [ ] **Step 3: Implement the collector.** Collect calls `fetch` under `context.WithTimeout(context.Background(), timeout)`. Use `timeout = 2*time.Second` in main.
- [ ] **Step 4: Wire it into main.** Add the flags `--inc-engine` (bool, true) and `--inc-engine-nodes` (comma list, default `strings.Join(incengine.DefaultNodes, ",")`). An empty list with `--inc-engine=true` is an error. The run dir is `filepath.Dir(*nbSock)`, or the path recorded in Task 1 if it differed. The fetch is `func(ctx) (string, error) { return appctl.ShowIncEngineStats(ctx, runDir) }`. Node mode only.
- [ ] **Step 5: Run the tests.** Run `go test ./...`. Expected: PASS, including `TestRunRejectsBadIncEngineNodes` and the existing `TestNodeModeServesMetricsWithoutSockets` (it now also exposes `ovnk_observ_appctl_errors_total`).
- [ ] **Step 6: Commit**

```bash
git add pkg/incengine cmd
git commit -m "feat(node): export northd inc-engine stats for allowlisted engine nodes"
```

---

### Task 7: MNP rule and peer counts

**Files:**
- Modify: `pkg/k8scount/transform.go`, `pkg/k8scount/collector.go`
- Test: `pkg/k8scount/transform_test.go`, `pkg/k8scount/collector_test.go`

**Interfaces:**
- Produces:
  - The private annotation keys `ovnk-observ.internal/ingress-rules`, `…/egress-rules`, `…/ingress-peers`, `…/egress-peers` (decimal strings), set by `transformMNP`. Rules = `len(spec.ingress)` / `len(spec.egress)`. Peers = Σ `len(from)` over ingress rules / Σ `len(to)` over egress rules.
  - Gauges `ovnkube_clustermanager_multi_network_policy_rules{namespace,direction}` and `…_peers{namespace,direction}`.

- [ ] **Step 1: Write the failing tests**
  - `TestTransformMNPKeepsCounts`: an MNP with 2 ingress rules (from lengths 3 and 0) and 1 egress rule (to length 4) yields annotations `2`, `1`, `3`, `4`. The cached object still has no `spec` and no `managedFields`. Update the existing "lacks other annotations" assertion so it allows the `ovnk-observ.internal/` keys.
  - `TestCollectorCountsMNPRulesAndPeers`: two MNPs in `ns1` give the expected summed series for both directions. An MNP with no `spec.ingress` emits ingress `0` and does not panic.
- [ ] **Step 2: Run the tests.** Run `go test ./pkg/k8scount/`. Expected: FAIL.
- [ ] **Step 3: Implement** the transform additions and the per-namespace sums in the existing `mnp` block of `Collect`.
- [ ] **Step 4: Run the tests.** Run `go test ./pkg/k8scount/`. Expected: PASS.
- [ ] **Step 5: Commit**

```bash
git add pkg/k8scount
git commit -m "feat(cluster): MultiNetworkPolicy rule and peer counts per namespace"
```

---

### Task 8: Churn counter with manager attribution

**Files:**
- Modify: `pkg/k8scount/transform.go`, `pkg/k8scount/collector.go`
- Create: `pkg/k8scount/churn.go`
- Test: `pkg/k8scount/churn_test.go`, `pkg/k8scount/collector_test.go`
- Modify: `cmd/ovnk-observ-exporter/main.go`, `cmd/ovnk-observ-exporter/main_test.go`

**Interfaces:**
- Consumes: `metrics.ParseNetworkLabelMode` (existing; parses `off` / `topN:<n>`).
- Produces:
  - `type Options struct{ ManagerLabels int }`: 0 means off.
  - `func NewCollector(ctx, dyn, kube, opts Options) (prometheus.Collector, error)` replaces the old signature. Update the call in `main.go` and the test helper `newCollector`.
  - Every transform sets the private annotation `ovnk-observ.internal/manager` to the `Manager` of the `managedFields` entry with the latest `Time`. When times are equal or nil, the last such entry in list order wins. With no entries, it is `unknown`.
  - `type churn struct` with `func (c *churn) record(resource, op, manager string)` and `func (c *churn) collect(ch chan<- prometheus.Metric, desc *prometheus.Desc)`.

**Manager label rule (refines spec §5 "top-N").** A counter series cannot move between label values without breaking `rate()`. So the first N distinct managers keep their own label for the life of the process, and every later manager counts under `_other`. With `off`, the label value is `""` (absent). Delete events use `manager="unknown"`, because the cached object names the last writer, not the deleter. Record this rule in spec §5 in Step 5.

- [ ] **Step 1: Write the failing tests**
  - `TestChurnFirstNManagersStable`: with N=2, record managers a, b, c, a, giving `{a:2, b:1, _other:1}`. Then a new manager d also lands in `_other`.
  - `TestChurnOffDropsManager`: with N=0, all events land in a series without a `manager` label.
  - `TestChurnSkipsUnchangedResourceVersion`: a handler update whose old and new `resourceVersion` match is not counted. A different `resourceVersion` counts as `update`.
  - `TestChurnSkipsInitialList`: objects present before start produce no `add` events. An MNP created after sync produces `ovnkube_clustermanager_object_events_total{resource="mnp",op="add",manager="<manager>"} 1`.
  - `TestTransformSetsLatestManager`: entries `[{m1, t1}, {m2, t2>t1}, {m3, nil}]` give `m2`. No entries give `unknown`.
- [ ] **Step 2: Run the tests.** Run `go test ./pkg/k8scount/`. Expected: FAIL.
- [ ] **Step 3: Implement the counter.** Register `cache.ResourceEventHandlerDetailedFuncs` on each informer before start: `AddFunc(obj, isInInitialList)` returns when `isInInitialList` is true; `UpdateFunc` compares resourceVersions; `DeleteFunc` unwraps `cache.DeletedFinalStateUnknown`. In main, add `--churn-manager-label` (default `topN:10`), parse it with `metrics.ParseNetworkLabelMode`, and pass `Options{ManagerLabels: mode.TopN}` (0 when `Off`). Add `TestRunRejectsBadChurnManagerLabel`.
- [ ] **Step 4: Run the tests.** Run `go test ./...`. Expected: PASS.
- [ ] **Step 5: Update the spec.** In spec §5, replace "top-N folded into `_other`" with the first-N rule above, and state that delete events use `manager="unknown"`.
- [ ] **Step 6: Commit**

```bash
git add pkg/k8scount cmd docs/superpowers/specs/2026-10-05-ovnk-observ-incident-amendment-design.md
git commit -m "feat(cluster): object churn counter with stable manager attribution"
```

---

### Task 9: Recording rules and alerts

**Files:**
- Modify: `rules/ovnk-observ-rules.yaml`
- Modify: `rules/tests/rules_test.yaml`
- Regenerate: `deploy/prometheusrule.yaml`

**Interfaces:**
- Produces the recording rules that Task 10 queries: `ovnk:ovn_pod_node:info`, `ovnk:pg_drift:missing_by_node`, `ovnk:pg_drift:nodes`, `ovnk:northd_recompute:ratio_15m{node,engine_node}`, `ovnk:northd_full_recompute:increase_5m{node}`, `ovnk:object_events:rate_5m{resource,op}`.

Rule bodies follow spec §7. Wrap node-mode exporter series in `max without (instance, pod, endpoint, container, service) (…)` before aggregating, as the existing rules do. Put `ovnk:ovn_pod_node:info` first in the group. Replace every `* on (namespace, pod) group_left(node) kube_pod_info` in the existing rules (txn failure ×2, e2e lag, staleness, DB deriv) with `* on (namespace, pod) group_left(node) ovnk:ovn_pod_node:info`. Alerts: `OVNKPortGroupSBDrift` (for 10m), `OVNKNorthdRecomputeHigh` (`> 0.5`, for 30m), and `OVNKPolicyChurnHigh` (`sum(ovnk:object_events:rate_5m{resource="mnp",op="update"}) * 60 > 100`, for 15m). All are `severity: warning` with a `summary` annotation in the existing style.

- [ ] **Step 1: Write the failing promtool tests** in `rules_test.yaml`:
  - Drift: two node-mode pods report `ovnkube_controller_port_group_sb_missing{node="A"}` at `3` (rollout overlap). Expect `ovnk:pg_drift:missing_by_node{node="A"} 3` and `ovnk:pg_drift:nodes 1`. The alert is absent at 9m and present at 11m. A second block with all nodes at 0 gives `ovnk:pg_drift:nodes 0`.
  - Recompute: on node A, `ovn_northd_inc_engine_runs_total{engine_node="lflow",type="recompute"}` increases by 60 and `type="compute"` by 40 over 15m. Expect a ratio of `0.6` and the alert firing after 30m.
  - Churn: `ovnkube_clustermanager_object_events_total{resource="mnp",op="update",manager="m"}` increases by 120 per minute. Expect `ovnk:object_events:rate_5m{resource="mnp",op="update"} 2` and the alert firing after 15m.
  - Join: `kube_pod_info` series in `openshift-ovn-kubernetes` and in `other-ns`. Expect `ovnk:ovn_pod_node:info` to contain only the OVN namespace. The existing `ovnk:nb_sb_e2e_lag_seconds` tests still pass unchanged.
- [ ] **Step 2: Run the tests.** Run `make rules-test`. Expected: FAIL on the new tests.
- [ ] **Step 3: Implement** the rules and alerts.
- [ ] **Step 4: Run the tests.** Run `make rules-test`. Expected: `SUCCESS`.
- [ ] **Step 5: Regenerate.** Run `make rules-gen`. Expected: `deploy/prometheusrule.yaml` changes only by the new and changed rules.
- [ ] **Step 6: Commit**

```bash
git add rules deploy/prometheusrule.yaml
git commit -m "feat(rules): drift, recompute, churn rules and alerts; join on OVN pods only"
```

---

### Task 10: Dashboard restructure

**Files:**
- Modify: `hack/dashgen/panels.go`, `hack/dashgen/main.go`, `hack/dashgen/panels_test.go`
- Modify: `Makefile` (`dashboard` target)
- Regenerate: `dashboards/ovn-scale-troubleshooting.json`, `deploy/dashboard-configmap.yaml`

**Interfaces:**
- Consumes: the Task 9 recording rule names, and the Task 4/6/7/8 metric names.
- Produces:
  - `type Options struct{ ACLLogURL, ACLLogTitle string }` and `func Build(o Options) Dashboard`. Update every `Build()` caller.
  - `join(expr)` returns `expr + " * on (namespace, pod) group_left(node) ovnk:ovn_pod_node:info{node=~\"$node\"}"`.
  - `func top(expr string) string { return "topk(10, " + expr + ")" }`.
  - main flags `-acl-log-url` and `-acl-log-title` (default title `ACL allow/deny`). The Makefile passes `-acl-log-url "$(ACL_LOG_URL)"` (empty by default).

Layout follows spec §6. The rows, in order:

| Row | Collapsed | Panels (span) |
|---|---|---|
| At a glance | no | Nodes with PG drift (2, crit ≥ 1, `ovnk:pg_drift:nodes`); Worst northd recompute ratio (2, percentunit, warnCrit(0.2, 0.5), `max(ovnk:northd_recompute:ratio_15m{engine_node=~"northd\|lflow"})`); Max NB→SB lag (1, s, warnCrit(10, 30)); Nodes disconnected (1, existing); Retry failures 15m (1, existing); ovnkube restarts 1h (1, warnCrit(1, 5), `sum(increase(kube_pod_container_status_restarts_total{namespace="openshift-ovn-kubernetes"}[1h]))`); Nodes not Ready (1, crit ≥ 1, `count(kube_node_status_condition{condition="Ready",status="true"} == 0) or vector(0)`); Cluster version (3, table, label `version`, `cluster_version{type="current"}`) |
| Consistency | no | Nodes with drift (6, table, labels `node`; columns Missing = `ovnk:pg_drift:missing_by_node > 0`, With ports, Ratio); PG drift over time (6, `top(ovnk:pg_drift:missing_by_node{node=~"$node"})`); northd recompute ratio (6, percentunit, `top(ovnk:northd_recompute:ratio_15m{node=~"$node"})`); Full-recompute events (6, `top(ovnk:northd_full_recompute:increase_5m{node=~"$node"})`, description "Includes manual inc-engine/recompute and northd restarts") |
| Change & churn | no | Object events/min by resource/op (6); Object events/min by manager (6); NB update rate by table/op (6, moved); Txn failure ratio (6, moved, `top(...)`) |
| Scale & fan-out | no | Max NB ACLs (2), Max NB PortGroups (2), Largest NB DB (2), PortGroup amplification (2, existing helper), ACLs per MNP rule (4, `max(sum by (node) (dedupe(nb_db_objects{table="ACL",owner_type="NetworkPolicy"}))) / clamp_min(sum(dedupe(rules)), 1)`); K8s network objects (4); MNP network targets (4); MNP rules & peers (4, `sum by (direction)` of each); Top networks by ACL/PortGroup (4, table); NB objects by node (6, table, `top(ovnk:nb_db_objects:sum_by_node_table{table=~"ACL\|Port_Group",node=~"$node"})`); SB/NB effectiveness (6, `top(ovnk:nb_sb_effectiveness:ratio{node=~"$node"})`); ACLs by owner type (6) |
| Programming latency | no | Programming p50/p99 1h (4); events/min (4); Resource add/update/delete p99 **1h** (4, `rate1h`); Pod setup p99 1h (4); Retry failures by node (4, `top`); NB→SB lag / staleness (4, `top`) |
| OVN internals | **yes** | Existing row 3 and row 5 panels, plus the raw northd and ovn-controller txn graphs. Wrap per-node targets in `top(...)`. Spans sum to a multiple of 12. |
| Exporter self-cost | **yes** | Existing 3 panels (3, 3, 6) plus appctl errors (12, `sum by (node) (rate(ovnk_observ_appctl_errors_total[$interval]))`) |

Removed: the "Network programming p99" singlestat (the row 4 graph covers it), "NB ACL vs SB Logical_Flow", "NB objects per node by table" (graph), and the old "Programming latency & backlog", "NB/SB DB health", "Transactions & churn", and "Recompute cost" row titles. Refresh changes from `1m` to `2m`.

- [ ] **Step 1: Update the tests first.**
  - `TestDashboardRows`: the 7 titles above in order. Collapsed must be true for exactly the last two.
  - `TestJoinHelper`: expects the new `join` string.
  - `TestNetworkProgrammingPanels` and the latency checks: assert `ovnk:ovn_pod_node:info{node=~"$node"}` instead of `kube_pod_info`.
  - Delete `TestProgrammingP99ValueMaps`.
  - `TestPanelCount`: set the new total.
  - New `TestRefreshTwoMinutes`.
  - New `TestNoKubePodInfoInPanels`: no target expression contains `kube_pod_info`.
  - New `TestPerNodeGraphsUseTopk`: every graph target whose legend contains `{{node}}` starts with `topk(10, `.
  - New `TestACLLogLink`: `Build(Options{})` has 1 link; `Build(Options{ACLLogURL: "https://example.invalid/acl"})` has 2, the second with `TargetBlank: true`.
  - New `TestResourceLatencyOneHour`: those targets contain `[1h]`.
- [ ] **Step 2: Run the tests.** Run `go test ./hack/dashgen/`. Expected: FAIL.
- [ ] **Step 3: Implement** the layout table above in `panels.go`, plus the `Options` plumbing in `main.go` and the Makefile.
- [ ] **Step 4: Run the tests.** Run `go test ./hack/dashgen/`. Expected: PASS (including the existing span, ID and singlestat-threshold checks).
- [ ] **Step 5: Regenerate.** Run `make dashboard`. Expected: both generated files change. Check `git diff --stat`.
- [ ] **Step 6: Commit**

```bash
git add hack/dashgen Makefile dashboards deploy/dashboard-configmap.yaml
git commit -m "feat(dashboard): consistency-first layout, topk per-node panels, OVN-pod join"
```

---

### Task 11: `verify-panels.sh` timings and idle panels

**Files:**
- Modify: `hack/verify-panels.sh`

- [ ] **Step 1: Time each query.** Have `query()` capture curl's `-w '\n%{time_total}'`: split the body from the trailing time and add the time to a per-panel total. Print lines as `%-8s %6dms  %s / %s`. The header comment documents the new column.
- [ ] **Step 2: Update the empty-panel rules.** Set `ALLOW_EMPTY_ROWS` to `"Exporter self-cost"` and match it as a list. Change `idle_rule` entries:
  - Remove "Network programming p99".
  - Add "Nodes with drift" → `ovnk:pg_drift:missing_by_node`.
  - Add "Full-recompute events" → `ovnk:northd_full_recompute:increase_5m`.
  - Add "Object events/min by resource/op" and "Object events/min by manager" → `ovnk_observ_informer_synced{resource="mnp"}`.
- [ ] **Step 3: Check the script.** Run `bash -n hack/verify-panels.sh && shellcheck hack/verify-panels.sh` (skip shellcheck if it isn't installed). Expected: no errors.
- [ ] **Step 4: Commit**

```bash
git add hack/verify-panels.sh
git commit -m "chore(verify-panels): per-panel query timing; idle rules for new panels"
```

---

### Task 12: Docs and install manifest

**Files:**
- Modify: `README.md` (Metrics: new flags and metric names; Limitations: northd `.ctl` discovery, drift needs both DBs synced)
- Modify: `INSTALL.md` (only if it lists flags or metrics)
- Modify: `docs/superpowers/specs/2026-10-05-ovnk-observ-incident-amendment-design.md`: set Status to "Implemented (pending lab gate)". In the §7 churn alert, use `resource="mnp"`. In §6, note that the programming p99 singlestat was removed and amplification moved to Scale & fan-out.
- Regenerate: `install/ovnk-observ.yaml`

- [ ] **Step 1: Make the doc edits** listed above.
- [ ] **Step 2: Render and verify the install manifest.** Run `make install-manifest && make verify-install-manifest`. Expected: exit 0. `VERSION` is not bumped; that is the user's call at release time.
- [ ] **Step 3: Commit**

```bash
git add README.md INSTALL.md docs/superpowers/specs install/ovnk-observ.yaml
git commit -m "docs: document drift, inc-engine and churn metrics; re-render install manifest"
```

---

### Task 13: Lab verification and the drift gate

Uses the cluster `oc` is logged into. Every step except Step 7 is read-only or touches only the `ovnk-observ` namespace.

- [ ] **Step 1: Confirm the cluster.** Run `oc whoami --show-server` and report it to the user.
- [ ] **Step 2: Record a baseline.** Before deploying, record each node exporter's memory and initial sync time:
  - Memory: `max_over_time(container_memory_working_set_bytes{namespace="ovnk-observ",container="exporter"}[1h])`, per pod.
  - Initial sync: `ovnk_observ_initial_sync_seconds`.
- [ ] **Step 3: Deploy.** Run `make deploy && make image-dev && make dashboard-apply`. Wait 2 minutes.
- [ ] **Step 4: Check drift against the bridge script.** Run the per-node NB/SB Port_Group diff script (spec §2) on two nodes. Compare its `missing=` with `ovnkube_controller_port_group_sb_missing{node=…}` from Thanos. Expected: equal (re-run once if churn moved either side).
- [ ] **Step 5: Check inc-engine values.** Run `ovn-appctl -t ovn-northd inc-engine/show-stats` on one node and compare `northd` and `lflow` with `ovn_northd_inc_engine_runs_total`. Expected: equal within one scrape interval of change.
- [ ] **Step 6: Apply the drift gate (spec §8).** After 1 hour, compare against Step 2:
  - Max working set ≤ 179 MiB (70% of 256 Mi).
  - Initial sync ≤ 1.25 × baseline.

  If either fails, change the `--pg-drift` default to `false` in `cmd/ovnk-observ-exporter/main.go`, update spec §5, README and any test asserting the default, then run `make install-manifest` and commit `fix(node): default --pg-drift off (failed lab gate: <numbers>)`. Report the numbers either way.
- [ ] **Step 7: Remediation check (ask the user first; this changes cluster state).** Only if some node shows drift > 0 and the user approves: run `ovn-appctl -t ovn-northd inc-engine/recompute` on that node. Confirm that the drift gauge reaches 0 within 2 scrapes and that "Full-recompute events" shows a point.
- [ ] **Step 8: Verify panels.** Run `make verify-panels`. Expected: `ERROR=0`, no EMPTY outside allowed rows, and no panel slower than 2000 ms. Report any slow panel.
- [ ] **Step 9: Check the dashboard in the browser.** Open Observe → Dashboards → "Networking / OVN-K Observ" in the browser (the user signs in). Take a screenshot of the first screen. Expand "OVN internals" and check in the browser network panel whether queries for collapsed rows fire only on expand. Record the answer in spec §9 in place of "Verify on the lab whether…".
- [ ] **Step 10: Commit any doc updates**

```bash
git add docs/superpowers/specs
git commit -m "docs(spec): lab results for drift gate and collapsed-row queries"
```
