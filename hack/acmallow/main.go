// Command acmallow merges the ovnk-observ metric names into the ACM hub's
// observability-metrics-custom-allowlist ConfigMap without dropping anything
// already in it. Applying deploy/acm/metrics-allowlist.yaml directly would
// replace the hub's list.
//
//	oc get cm observability-metrics-custom-allowlist -n open-cluster-management-observability -o yaml --ignore-not-found \
//	  | go run ./hack/acmallow -add deploy/acm/metrics-allowlist.yaml > merged.yaml
//
// Existing names keep their order and new names are appended. Every other key
// in metrics_list.yaml (recording_rules, matches, ...) and every other data key
// is kept. Server-managed metadata is dropped except resourceVersion, so an
// edit made in the meantime makes the apply fail rather than being lost.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"
)

const (
	cmName  = "observability-metrics-custom-allowlist"
	cmNS    = "open-cluster-management-observability"
	listKey = "metrics_list.yaml"
)

type ConfigMap struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   Metadata          `json:"metadata"`
	Data       map[string]string `json:"data"`
}

type Metadata struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace"`
	ResourceVersion string            `json:"resourceVersion,omitempty"`
	Labels          map[string]string `json:"labels,omitempty"`
	Annotations     map[string]string `json:"annotations,omitempty"`
}

func names(l map[string]any) ([]string, error) {
	raw, _ := l["names"].([]any)
	if l["names"] != nil && raw == nil {
		return nil, fmt.Errorf("%s: names is not a list", listKey)
	}
	out := make([]string, 0, len(raw))
	for _, n := range raw {
		s, ok := n.(string)
		if !ok {
			return nil, fmt.Errorf("%s: name %v is not a string", listKey, n)
		}
		out = append(out, s)
	}
	return out, nil
}

func parse(b []byte) (ConfigMap, map[string]any, error) {
	var cm ConfigMap
	if err := yaml.Unmarshal(b, &cm); err != nil {
		return cm, nil, err
	}
	if cm.Kind != "ConfigMap" || cm.Metadata.Name != cmName || cm.Metadata.Namespace != cmNS {
		return cm, nil, fmt.Errorf("want ConfigMap %s/%s, got %s %s/%s", cmNS, cmName, cm.Kind, cm.Metadata.Namespace, cm.Metadata.Name)
	}
	l := map[string]any{}
	if err := yaml.Unmarshal([]byte(cm.Data[listKey]), &l); err != nil {
		return cm, nil, fmt.Errorf("%s: %w", listKey, err)
	}
	if l == nil {
		l = map[string]any{}
	}
	return cm, l, nil
}

// Merge returns live with ours' names added, and the names it added. An empty
// live means the hub has no allowlist yet; ours is used as the base.
func Merge(live, ours []byte) ([]byte, []string, error) {
	_, oursList, err := parse(ours)
	if err != nil {
		return nil, nil, fmt.Errorf("ovnk-observ allowlist: %w", err)
	}
	want, err := names(oursList)
	if err != nil {
		return nil, nil, err
	}

	fresh := len(bytes.TrimSpace(live)) == 0
	base := live
	if fresh {
		base = ours
	}
	cm, l, err := parse(base)
	if err != nil {
		return nil, nil, fmt.Errorf("live allowlist: %w", err)
	}
	have, err := names(l)
	if err != nil {
		return nil, nil, err
	}

	seen := map[string]bool{}
	for _, n := range have {
		seen[n] = true
	}
	var added []string
	if fresh {
		added = want
	} else {
		for _, n := range want {
			if !seen[n] {
				have = append(have, n)
				added = append(added, n)
				seen[n] = true
			}
		}
	}
	text := cm.Data[listKey]
	if !fresh {
		if text, err = appendNames(text, added); err != nil {
			return nil, nil, err
		}
		// The text edit must parse back to exactly the merged list.
		var check map[string]any
		if err := yaml.Unmarshal([]byte(text), &check); err != nil {
			return nil, nil, fmt.Errorf("%s: merged text does not parse: %w", listKey, err)
		}
		if got, err := names(check); err != nil || !slices.Equal(got, have) {
			return nil, nil, fmt.Errorf("%s: merged names do not match; merge by hand", listKey)
		}
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[listKey] = text
	delete(cm.Metadata.Annotations, "kubectl.kubernetes.io/last-applied-configuration")
	if len(cm.Metadata.Annotations) == 0 {
		cm.Metadata.Annotations = nil
	}
	out, err := yaml.Marshal(cm)
	return out, added, err
}

// appendNames adds names after the last item of the top-level block-style
// "names:" list in text, matching that item's indentation, and leaves every
// other line untouched so a diff shows only the additions. With no "names:"
// key it prepends one.
func appendNames(text string, add []string) (string, error) {
	if len(add) == 0 {
		return text, nil
	}
	lines := strings.SplitAfter(text, "\n")
	start := -1
	for i, ln := range lines {
		if k, v, ok := strings.Cut(strings.TrimRight(ln, "\r\n"), ":"); ok && k == "names" {
			if strings.TrimSpace(v) != "" {
				return "", fmt.Errorf("%s: names is not a block list (%q); merge by hand", listKey, strings.TrimSpace(v))
			}
			start = i
			break
		}
	}
	item := func(prefix string) string {
		var b strings.Builder
		for _, n := range add {
			b.WriteString(prefix + n + "\n")
		}
		return b.String()
	}
	if start < 0 {
		return "names:\n" + item("  - ") + text, nil
	}
	at, prefix := start+1, "  - "
	for i := start + 1; i < len(lines); i++ {
		t := strings.TrimLeft(lines[i], " ")
		if strings.TrimSpace(t) == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if !strings.HasPrefix(t, "- ") {
			break // next key
		}
		at, prefix = i+1, lines[i][:len(lines[i])-len(t)]+"- "
	}
	if at > 0 && !strings.HasSuffix(lines[at-1], "\n") {
		lines[at-1] += "\n"
	}
	out := append(append(append([]string{}, lines[:at]...), item(prefix)), lines[at:]...)
	return strings.Join(out, ""), nil
}

func main() {
	add := flag.String("add", "deploy/acm/metrics-allowlist.yaml", "ovnk-observ allowlist ConfigMap whose names are merged in")
	flag.Parse()

	ours, err := os.ReadFile(*add)
	if err == nil {
		var live, out []byte
		var added []string
		if live, err = io.ReadAll(os.Stdin); err == nil {
			if out, added, err = Merge(live, ours); err == nil {
				_, err = os.Stdout.Write(out)
				if len(added) == 0 {
					fmt.Fprintln(os.Stderr, "acmallow: all ovnk-observ names already present; nothing to apply")
				} else {
					fmt.Fprintf(os.Stderr, "acmallow: adding %d name(s): %s\n", len(added), strings.Join(added, ", "))
				}
			}
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "acmallow:", err)
		os.Exit(1)
	}
}
