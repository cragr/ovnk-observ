#!/usr/bin/env bash
# Runs the ovsdbmon integration test against a real ovsdb-server inside a
# throwaway pod in ovnk-observ, using the nbdb image of the ovnkube-node pods.
set -euo pipefail

NS=ovnk-observ
POD=ovsdbmon-it-$(date +%s)
cd "$(dirname "$0")/../.."

cleanup() { oc delete pod "$POD" -n "$NS" --ignore-not-found --wait=true || true; }
trap cleanup EXIT

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -c -tags integration -o bin/ovsdbmon.test ./pkg/ovsdbmon

image=$(oc get pods -n openshift-ovn-kubernetes -l app=ovnkube-node \
	-o jsonpath='{.items[0].spec.containers[?(@.name=="nbdb")].image}')
[[ -n "$image" ]] || { echo "could not find nbdb image" >&2; exit 2; }

cat <<YAML | oc apply -n "$NS" -f -
apiVersion: v1
kind: Pod
metadata:
  name: $POD
spec:
  restartPolicy: Never
  activeDeadlineSeconds: 600
  containers:
  - name: it
    image: $image
    command: ["sleep", "infinity"]
YAML
oc wait pod/"$POD" -n "$NS" --for=condition=Ready --timeout=180s

oc cp bin/ovsdbmon.test "$NS/$POD:/tmp/ovsdbmon.test" -c it
out=$(mktemp)
trap 'rm -f "$out"; cleanup' EXIT
oc exec -n "$NS" "$POD" -c it -- sh -c 'chmod +x /tmp/ovsdbmon.test && /tmp/ovsdbmon.test -test.v -test.run TestMonitorAgainstRealOVSDBServer' | tee "$out"
# A skipped or missing test must not read as success.
grep -q -- '--- PASS: TestMonitorAgainstRealOVSDBServer' "$out" || { echo "integration test did not report PASS" >&2; exit 1; }
