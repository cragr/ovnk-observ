# ovnk-observ

A prototype OVN-Kubernetes scale exporter and console dashboard for OpenShift. A per-node exporter reads the local NB and SB databases over their unix sockets (monitor only, never `transact`) and counts rows by table, owner type, and network. A cluster-wide exporter counts NADs, MultiNetworkPolicies, NetworkPolicies, and user-defined networks from the Kubernetes API. Recording rules, alerts, and an OpenShift console dashboard turn those counts into a scale and troubleshooting view.

## Why

A lab with 3,150 MultiNetworkPolicies, each targeting 147 NADs, produced about 464k ACLs and 475k Port_Groups. The NB database grew to roughly 1 GB. Existing OVN-Kubernetes metrics did not show which objects drove that growth. This project adds those metrics.

## Install

Cluster admins: follow [INSTALL.md](INSTALL.md). It applies one prebuilt manifest, `install/ovnk-observ.yaml`, and needs no build.

## Develop

Prerequisites: `oc` logged in as cluster-admin, Go 1.26, and podman (for release images only).

```
make build                    # compile bin/ovnk-observ-exporter (linux/amd64)
make test                     # unit tests
make deploy                   # dev: deploy/ plus the in-cluster BuildConfig (overlays/dev)
make image-dev                # build the binary, build the image in-cluster, roll the pods onto it
make rules-gen                # regenerate deploy/prometheusrule.yaml from rules/
make dashboard                # regenerate dashboards/ and deploy/dashboard-configmap.yaml
make dashboard-apply          # apply the dashboard ConfigMap to openshift-config-managed
make undeploy                 # remove everything, including the dashboard
```

`make deploy` creates the BuildConfig but does not build the image. Run `make image-dev` after every `make deploy` (re-applying resets the image to the short name) and after any `make undeploy`. It pins the pod templates to the ImageStream's digest, because image policy admission did not resolve the short name on the lab. The pods start once the first build lands in the ImageStream.

After `make rules-gen` or `make dashboard`, run `make install-manifest` to refresh the install file, and `make deploy` to apply the change to a dev cluster.

The PriorityClass uses `preemptionPolicy: Never`. The field is immutable, so on a cluster deployed before that change, run `oc delete priorityclass ovnk-observ` before `make deploy`.

### Release

```
make image-build              # podman build quay.io/cragr/ovnk-observ-exporter:$(VERSION) and :latest
make image-push               # push both tags
make install-manifest         # render install/ovnk-observ.yaml; commit the result
make verify-install-manifest  # fail if install/ovnk-observ.yaml is stale or not on $(VERSION)
make deploy-release           # apply install/ovnk-observ.yaml, as an admin would
```

`VERSION` defaults to `v0.1.0` and `IMG` to `quay.io/cragr/ovnk-observ-exporter`. The image tag lives only in the `images:` entry of `deploy/kustomization.yaml`. To cut a release, bump `VERSION` in the Makefile and run `make install-manifest`; it runs `make set-version` to update that entry, then renders the file. `make verify-install-manifest` changes no files and fails if the rendered file is stale or if the DaemonSet and Deployment do not both use `$(IMG):$(VERSION)`.

The install manifest is rendered with kustomize `$(KUSTOMIZE_VERSION)` (v5.8.1), which `make tools` downloads to `bin/` alongside promtool. Other kustomize versions may format the output differently and make the verify step fail.

## Metrics

The full catalog, with labels and the cardinality guard, is in [spec section 5](docs/superpowers/specs/2026-10-02-ovnk-observ-dashboard-design.md#5-metric-catalog). The incident amendment adds the metrics below ([amendment section 5](docs/superpowers/specs/2026-10-05-ovnk-observ-incident-amendment-design.md#5-metric-catalog-additions)). Exporter metrics are served on `:9410/metrics`. Recording rules and alerts are in `rules/ovnk-observ-rules.yaml`.

New exporter flags:

| Flag | Mode | Default | Effect |
|---|---|---|---|
| `--pg-drift` | node | `true` | Monitor NB `Port_Group.ports` and SB `Port_Group.name`; export the drift gauges |
| `--inc-engine` | node | `true` | Read northd `inc-engine/show-stats` on each scrape |
| `--inc-engine-nodes` | node | `northd,lflow,port_group,sync_to_sb_addr_set,sync_from_sb,ls_stateful,lr_stateful` | Engine nodes to export |
| `--churn-manager-label` | cluster | `topN:10` | `off` drops the `manager` label; `topN:<n>` keeps the first n distinct managers |

New metrics:

| Metric | Labels |
|---|---|
| `ovnkube_controller_port_group_sb_missing` | none (node comes from the scrape target) |
| `ovnkube_controller_port_group_with_ports` | none |
| `ovn_northd_inc_engine_runs_total` | `engine_node`, `type` (`recompute`, `compute`, `cancel`) |
| `ovnkube_clustermanager_object_events_total` | `resource` (`nad`, `mnp`, `networkpolicy`, `udn`, `cudn`), `op` (`add`, `update`, `delete`), `manager` |
| `ovnkube_clustermanager_multi_network_policy_rules` | `namespace`, `direction` |
| `ovnkube_clustermanager_multi_network_policy_peers` | `namespace`, `direction` |
| `ovnk_observ_appctl_errors_total` | `command` |

New recording rules: `ovnk:ovn_pod_node:info`, `ovnk:pg_drift:missing_by_node`, `ovnk:pg_drift:nodes`, `ovnk:northd_recompute:ratio_15m`, `ovnk:northd_full_recompute:increase_5m`, `ovnk:object_events:rate_5m`. New alerts: `OVNKPortGroupSBDrift`, `OVNKNorthdRecomputeHigh`, `OVNKPolicyChurnHigh`.

The only appctl command the exporter sends is `inc-engine/show-stats`. It is read-only, and no other command is reachable.

## Dashboard

In the OpenShift console, open Observe > Dashboards and select **Networking / OVN-K Observ**. The dashboard is generated by `make dashboard` (source: `hack/dashgen`, output: `dashboards/` and `deploy/dashboard-configmap.yaml`).

The console renders `gauge` panels as plain numbers, so the dashboard has no dials. Single-stat colors come from `options.fieldOptions.thresholds`, and critical uses `light-red`, the strongest red in the console palette (`red` renders orange).

## Verification

```
make verify-counts    # compare exporter row counts with ovsdb-server on one node (NODE=<node>)
make verify-panels    # run every dashboard panel query against Thanos and report OK, OK(idle), EMPTY, or ERROR
make test             # unit tests
make rules-test       # promtool rule tests
make integration      # ovsdbmon against a real ovsdb-server
```

After a deploy or rollout, allow 1 to 2 minutes before running `verify-counts` or `verify-panels`. The node exporters need tens of seconds for the initial NB dump, and Prometheus needs a scrape or two after that.

## Limitations

- Prototype. The metrics are named for upstream adoption but are not in OVN-Kubernetes today.
- The cluster exporter discovers CRDs once at start. A CRD installed later (for example, UDN) or a transient discovery error leaves that resource unwatched until the cluster pod restarts.
- The node DaemonSet hard-codes the OVN-IC hostPath `/var/run/ovn-ic`. It does not work in non-IC (central) mode.
- northd `.ctl` discovery reads `ovn-northd.pid` beside the NB socket and connects to `ovn-northd.<pid>.ctl`. This is not yet confirmed on a live cluster. If it fails, inc-engine series are absent and `ovnk_observ_appctl_errors_total` increases.
- Drift is exported only while both NB and SB are synced. During the initial dump or a reconnect the series are absent, not zero.
- The churn `manager` label is first-N: the first N distinct managers seen keep their own value for the life of the process, and later ones count under `_other`. The assignment resets on restart.
- The initial dump of about 940k rows takes tens of seconds, during which the object series are absent.

## Upstream path

See [spec section 11](docs/superpowers/specs/2026-10-02-ovnk-observ-dashboard-design.md#11-upstream-path). In short: move the counters into ovnkube-controller and ovnkube-cluster-manager under the same metric names, then drop this exporter.

## Verification notes

Run on 2026-10-02 against the lab cluster (3,150 MNPs in `<repro-namespace>`, 147 NADs each).

**Scale delta.** The test deleted 10 MNPs, waited for `ovnk:nb_db_objects:max_by_table` to settle, then recreated them from a backup.

| Table | Before | After delete | After restore |
|---|---|---|---|
| Port_Group | 474,940 | 473,470 | 474,940 |
| ACL | 464,102 | 462,632 | 464,102 |

- Port_Group drop: 1,470. Expected: 10 x 147 x (474,940 / 463,050) = 1,507.8. The result is 2.5% below the expected value, inside the 10% tolerance.
- ACL dropped by the same 1,470.
- The 10 MNPs were restored from the backup with server-set fields stripped. MNP count returned to 3,150, and each restored policy's `policy-for` annotation matched the saved copy.

**Undeploy and redeploy.** `make undeploy` removed the namespace, ClusterRoles, ClusterRoleBindings, PriorityClass, and dashboard ConfigMap. The ClusterRoles were confirmed absent by listing them, not by a NotFound lookup. After `make deploy`, `make image` (now `make image-dev`), and `make dashboard-apply`, all pods rolled out. `hack/verify-counts.sh` matched ovsdb-server exactly (ACL 464,102; Port_Group 474,940), and `hack/verify-panels.sh` reported OK=35, EMPTY=0, ERROR=0.

**Final fix wave.** After the dashboard split (37 panels), the amplification rule fix, the `monitor_canceled` reconnect, and the deploy hardening (read-only socket mount, no service account token on node pods, `preemptionPolicy: Never`), all pods rolled out on the new image. `ovnk_observ_db_connected` was 1 for nb and sb on all 5 nodes, `hack/verify-counts.sh <worker-node>` matched ovsdb-server (ACL 464,102; Port_Group 474,940), and `hack/verify-panels.sh` reported OK=37, EMPTY=0, ERROR=0.
