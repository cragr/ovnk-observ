#!/usr/bin/env bash
# verify-counts.sh NODE
#
# Compares the exporter's ACL and Port_Group counts for NODE (as stored in the
# platform Prometheus, queried through Thanos) with a live read-only select of
# the node's OVN NB DB. Exits non-zero if any table differs.
set -euo pipefail

NODE="${1:?usage: $0 NODE}"
OVN_NS=openshift-ovn-kubernetes
NB_SOCK=unix:/var/run/ovn/ovnnb_db.sock
TABLES=(ACL Port_Group)

pod=$(oc get pods -n "$OVN_NS" -l app=ovnkube-node \
	--field-selector "spec.nodeName=${NODE}" -o jsonpath='{.items[0].metadata.name}')
[[ -n "$pod" ]] || { echo "no ovnkube-node pod on node ${NODE}" >&2; exit 2; }

thanos_host=$(oc get route thanos-querier -n openshift-monitoring -o jsonpath='{.spec.host}')
token=$(oc whoami -t)

ovsdb_count() {
	oc exec -n "$OVN_NS" "$pod" -c nbdb -- ovsdb-client transact "$NB_SOCK" \
		"[\"OVN_Northbound\",{\"op\":\"select\",\"table\":\"$1\",\"where\":[],\"columns\":[\"_uuid\"]}]" \
		| grep -o _uuid | wc -l | tr -d ' '
}

query="sum by (table) (ovnkube_controller_nb_db_objects{node=\"${NODE}\",table=~\"$(IFS='|'; echo "${TABLES[*]}")\"})"
resp=$(curl -sfk -H "Authorization: Bearer ${token}" "https://${thanos_host}/api/v1/query" \
	--data-urlencode "query=${query}")

echo "node=${NODE} ovnkube-node pod=${pod}"
rc=0
for t in "${TABLES[@]}"; do
	exp=$(jq -r --arg t "$t" '.data.result[] | select(.metric.table==$t) | .value[1]' <<<"$resp")
	db=$(ovsdb_count "$t")
	if [[ -z "$exp" ]]; then
		echo "$t MISSING exporter=<no series> ovsdb=${db}"
		rc=1
	elif [[ "$exp" == "$db" ]]; then
		echo "$t match exporter=${exp} ovsdb=${db}"
	else
		echo "$t MISMATCH exporter=${exp} ovsdb=${db}"
		rc=1
	fi
done
exit "$rc"
