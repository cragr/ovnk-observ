# Install ovnk-observ on OpenShift

ovnk-observ adds OVN-Kubernetes scale metrics, alerts, and a console dashboard to an OpenShift cluster. You install one prebuilt manifest. You do not need Go, a container build, or a clone of the repo.

## 1. Before you start

You need:

- OpenShift 4.20 or later with OVN-Kubernetes in interconnect (IC) mode. IC is the default since 4.14.
- A user with `cluster-admin`.
- The `oc` CLI, logged in to the cluster.
- Pull access to `quay.io/cragr/ovnk-observ-exporter:v0.2.0`. The quay.io repository is public, so a connected cluster needs no pull secret. On a disconnected cluster, mirror the image to your registry and add an `ImageTagMirrorSet` for `quay.io/cragr/ovnk-observ-exporter` before you install. The manifest pulls by tag, and an `ImageDigestMirrorSet` applies only to pulls by digest.

The manifest creates:

- Namespace `ovnk-observ`, labeled for platform monitoring.
- DaemonSet `ovnk-observ-node`: one privileged pod per node, bound to the `privileged` SCC. It mounts the host directory `/var/run/ovn-ic` read-only and reads the local NB and SB databases through their unix sockets. It only monitors; it never writes to the databases.
- Deployment `ovnk-observ-cluster`: one pod with read-only (`get`, `list`, `watch`) cluster RBAC on NetworkAttachmentDefinitions, MultiNetworkPolicies, NetworkPolicies, UserDefinedNetworks, and ClusterUserDefinedNetworks.
- PriorityClass `ovnk-observ` (value 1000000, below the system classes, never preempts).
- Two Services and two ServiceMonitors, plus a Role that lets the platform Prometheus scrape them.
- PrometheusRule `ovnk-observ` with recording rules and alerts.
- ConfigMap `grafana-dashboard-ovn-scale-troubleshooting` in `openshift-config-managed` (the console dashboard).

## 2. Install

Apply the manifest from the release tag (`v0.2.0` is the current release):

```
oc apply -f https://raw.githubusercontent.com/cragr/ovnk-observ/v0.2.0/install/ovnk-observ.yaml
```

The repository is not published yet, so this URL does not work until it is. Until then, apply a local copy of `install/ovnk-observ.yaml`:

```
oc apply -f install/ovnk-observ.yaml
```

## 3. Verify

1. Wait for the rollouts:

   ```
   oc rollout status ds/ovnk-observ-node -n ovnk-observ --timeout=5m
   oc rollout status deploy/ovnk-observ-cluster -n ovnk-observ --timeout=5m
   oc get pods -n ovnk-observ -o wide
   ```

   Expect one `ovnk-observ-node` pod per node and one `ovnk-observ-cluster` pod, all `Running`.

2. Wait 1 to 2 minutes. Each node pod dumps its NB database at start; a large NB database (about 1 GB) takes about 30 seconds. Prometheus then needs a scrape or two.

3. In the console, open **Observe > Dashboards** and select **Networking / OVN-K Observ**.

4. In **Observe > Metrics**, run:

   ```
   ovnk_observ_db_connected
   ```

   Expect two series per node (`db="nb"` and `db="sb"`), each with value 1.

## 4. What the dashboard shows

- **PortGroup amplification**: NB Port_Groups owned by policies, divided by the number of NetworkPolicies plus MultiNetworkPolicies. A high ratio means each policy fans out across many networks (for example, one MNP that targets 147 NADs).
- **NB objects per node, top networks**: ACL and Port_Group counts by node, table, owner type, and network. These show which policies and networks drive NB growth.
- **NB ACL vs SB Logical_Flow**: compares what ovnkube writes to NB with what northd compiles into SB. A gap that grows faster than NB points at northd cost, not policy count.
- **Network programming latency**: p50 and p99 over a 1-hour window. The p99 stat shows "idle" when nothing was programmed in the last hour; that is normal on a quiet cluster.
- **DB size and growth**: on-disk size of the NB and SB databases and the growth rate per hour.
- **Transaction failures**: northd and ovn-controller transaction rates by result, and the failure ratio.
- **Recompute cost**: northd loop time, lflow build time, ovn-controller lflow runs, and br-int OpenFlow count.
- **Exporter self-cost**: CPU and memory of the exporter pods and their DB connection state.

Alerts (all `severity: warning`):

| Alert | Fires when |
|---|---|
| OVNKPortGroupAmplificationHigh | amplification ratio > 10 for 15m |
| OVNKNBDBObjectsHigh | NB ACL or Port_Group count on a node > 100,000 for 15m |
| OVNKNBDBGrowing | NB database grows > 50 MB/h for 1h |
| OVNKPropagationLagHigh | NB to SB propagation lag > 30s for 15m |
| OVNKE2EProbeStale | E2E probe not updated for > 180s for 15m |
| OVNKTxnFailureRatioHigh | OVN transaction failure ratio > 5% for 15m |
| OVNKNetworkProgrammingSlow | network programming p99 (1h window) > 10s for 15m |
| OVNKPortGroupSBDrift | a node has NB Port_Groups with ports that are missing from the SB DB for 10m |
| OVNKNorthdRecomputeHigh | northd or lflow recomputes > 50% of the time over 15m, for 30m |
| OVNKPolicyChurnHigh | MultiNetworkPolicy updates > 100 per minute for 15m |

## 5. Tune

Re-applying the manifest reverts in-cluster edits. To keep a change, download `ovnk-observ.yaml`, edit your copy, and apply that.

- **Per-network labels**: the node DaemonSet runs with `--per-network-labels=topN:50`. It keeps a `network` label for the 50 largest networks per table and owner type and sums the rest under `_other`. Lower N to cut series count, raise it for more detail, or set `off` to leave the label empty.
- **Drift, inc-engine, churn**: the node DaemonSet runs with `--pg-drift=true` and `--inc-engine=true`; set either to `false` to turn it off. `--inc-engine-nodes` is the list of northd engine nodes exported. The cluster Deployment runs with `--churn-manager-label=topN:10`, which keeps a `manager` label for the first 10 distinct writers and counts later ones under `_other`; `off` drops the label. `--pg-drift` is expected to have the largest cost (see the spec §8 gate; it monitors the `ports` column), so turn it off first if node memory is tight.
- **Resources**: node pods request 50m CPU and 64Mi and are limited to 500m and 256Mi. The cluster pod requests 20m and 64Mi, limited to 200m and 256Mi. On a cluster with a very large NB database, watch the "Exporter memory" panel and raise the node memory limit if it nears 256Mi.
- **Alert thresholds**: edit the `alert` rules in the `ovnk-observ` PrometheusRule, or run `oc edit prometheusrule ovnk-observ -n ovnk-observ`.

## 6. Uninstall

Run one of these, matching how you installed:

```
# From the release URL
oc delete --ignore-not-found -f https://raw.githubusercontent.com/cragr/ovnk-observ/v0.2.0/install/ovnk-observ.yaml

# From a local copy
oc delete --ignore-not-found -f install/ovnk-observ.yaml
```

`--ignore-not-found` makes the command safe to re-run. It removes the namespace and everything in it, the cluster RBAC, the PriorityClass, and the dashboard ConfigMap in `openshift-config-managed`.

## 7. Troubleshoot

- **Node pods stay Pending or the DaemonSet reports a SCC error**: check `oc get events -n ovnk-observ`. The pods need the `privileged` SCC through ClusterRoleBinding `ovnk-observ-node-scc`. Confirm that binding exists and that no admission policy blocks privileged pods in `ovnk-observ`.
- **ImagePullBackOff**: the cluster cannot pull `quay.io/cragr/ovnk-observ-exporter:v0.2.0`. The quay.io repository is public, so a connected cluster needs no pull secret; check egress to quay.io. If quay.io is not reachable, mirror the image and add an `ImageTagMirrorSet` (see section 1); an `ImageDigestMirrorSet` does not cover this tag pull. If your mirror registry requires credentials, add them to the cluster global pull secret.
- **`ovnk_observ_db_connected` is 0**: the node pod cannot reach the OVN sockets. The DaemonSet expects IC mode, where the sockets live in `/var/run/ovn-ic` on the host. A cluster without IC (central NB/SB) is not supported. Check the node pod log: `oc logs -n ovnk-observ ds/ovnk-observ-node`.
- **Dashboard panels are empty right after install**: wait 1 to 2 minutes for the initial NB dump and the first scrapes. Rate and latency panels need several minutes of data. The "Network programming p99" stat shows "idle" until something is programmed.
