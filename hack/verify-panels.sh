#!/usr/bin/env bash
# verify-panels.sh [DASHBOARD_JSON]
#
# Runs every panel query of the generated dashboard as an instant query
# against thanos-querier ($node/$network -> .*, $interval -> 5m) and prints
# one line per panel:
#
#   OK|EMPTY|ERROR  <ms>ms  <row> / <panel title>
#
# <ms> is the total wall time of all of the panel's queries (plus the idle
# re-check, if any), taken from curl's time_total and rounded to integer ms.
#
# followed by an indented line for every target that errored or returned no
# series. A panel is EMPTY when all its targets return no series; a series
# whose value is NaN (e.g. an idle histogram_quantile) counts as data.
# Exits 1 on any ERROR, or on an EMPTY panel outside the ALLOW_EMPTY_ROWS rows
# ("Exporter self-cost").
#
# Idle-allowed panels (see idle_rule) filter out NaN on purpose, so they are
# legitimately empty when nothing happened. For those, an empty panel is
# re-checked against the underlying recording rule: if the rule returns a
# series (NaN counts), the panel is reported OK(idle); if the rule returns
# nothing (e.g. not loaded) the panel stays EMPTY and fails.
set -euo pipefail

DASH="${1:-$(dirname "$0")/../dashboards/ovn-scale-troubleshooting.json}"
ALLOW_EMPTY_ROWS=("Exporter self-cost")

# row_allowed succeeds when $1 is listed in ALLOW_EMPTY_ROWS.
row_allowed() {
	local r
	for r in "${ALLOW_EMPTY_ROWS[@]}"; do
		[[ "$r" == "$1" ]] && return 0
	done
	return 1
}

command -v jq >/dev/null || { echo "jq is required" >&2; exit 2; }
thanos_host=$(oc get route thanos-querier -n openshift-monitoring -o jsonpath='{.spec.host}')
token=$(oc whoami -t)

# idle_rule prints the recording rule behind an idle-allowed panel title.
idle_rule() {
	case "$1" in
	"Nodes with drift") echo "ovnk:pg_drift:missing_by_node" ;;
	"Full-recompute events") echo "ovnk:northd_full_recompute:increase_5m" ;;
	"Object events/min by resource/op" | "Object events/min by manager" | "MNP rules & peers" | "ACLs per MNP rule")
		echo 'ovnk_observ_informer_synced{resource="mnp"}' ;;
	esac
}

# split_timed splits curl output ("<body>\n<seconds>") into the globals QBODY
# and QMS (integer milliseconds; 0 if the time is missing or malformed).
split_timed() {
	local raw=$1 secs=0
	if [[ "$raw" == *$'\n'* ]]; then
		QBODY=${raw%$'\n'*}
		secs=${raw##*$'\n'}
	else
		QBODY=$raw
	fi
	QMS=$(awk -v s="$secs" 'BEGIN { if (s !~ /^[0-9.]+$/) s = 0; printf "%d", s * 1000 + 0.5 }')
}

# query runs one instant query; sets QBODY and QMS (see split_timed).
query() {
	local raw
	raw=$(curl -sk -w '\n%{time_total}' -H "Authorization: Bearer ${token}" \
		"https://${thanos_host}/api/v1/query" --data-urlencode "query=$1" || true)
	split_timed "$raw"
}

ok=0 idle=0 empty=0 error=0 rc=0
while IFS= read -r panel; do
	row=$(jq -r '.row' <<<"$panel")
	title=$(jq -r '.title' <<<"$panel")
	n=$(jq '.exprs | length' <<<"$panel")
	notes=() got=0 bad=0 total_ms=0
	for ((i = 0; i < n; i++)); do
		expr=$(jq -r --argjson i "$i" '.exprs[$i]' <<<"$panel")
		expr=${expr//\$node/.*}
		expr=${expr//\$network/.*}
		expr=${expr//\$interval/5m}
		query "$expr"
		resp=$QBODY total_ms=$((total_ms + QMS))
		status=$(jq -r '.status // "error"' <<<"$resp" 2>/dev/null || echo error)
		if [[ "$status" != "success" ]]; then
			bad=1
			notes+=("    target $i ERROR: $(jq -r '.error // "no response"' <<<"$resp" 2>/dev/null || echo "$resp" | head -c 200)")
			continue
		fi
		count=$(jq '.data.result | length' <<<"$resp")
		if ((count > 0)); then
			got=1
		else
			notes+=("    target $i empty: $expr")
		fi
	done
	if ((bad)); then
		verdict=ERROR error=$((error + 1)) rc=1
	elif ((got)); then
		verdict=OK ok=$((ok + 1))
	else
		verdict=EMPTY
		rule=$(idle_rule "$title")
		if [[ -n "$rule" ]]; then
			query "$rule"
			resp=$QBODY total_ms=$((total_ms + QMS))
			if [[ "$(jq -r '.status // "error"' <<<"$resp" 2>/dev/null || echo error)" == "success" ]] &&
				(($(jq '.data.result | length' <<<"$resp") > 0)); then
				verdict="OK(idle)"
				notes=("    idle: $rule = $(jq -r '.data.result[0].value[1]' <<<"$resp")")
			else
				notes+=("    idle check failed: $rule returned no series")
			fi
		fi
		if [[ "$verdict" == "OK(idle)" ]]; then
			idle=$((idle + 1))
		else
			empty=$((empty + 1))
			row_allowed "$row" || rc=1
		fi
	fi
	printf '%-8s %6dms  %s / %s\n' "$verdict" "$total_ms" "$row" "$title"
	for line in ${notes[@]+"${notes[@]}"}; do printf '%s\n' "$line"; done
done < <(jq -c '.rows[] | .title as $r | .panels[] | {row: $r, title, exprs: [.targets[].expr]}' "$DASH")

echo "panels: OK=${ok} OK(idle)=${idle} EMPTY=${empty} ERROR=${error}"
exit "$rc"
