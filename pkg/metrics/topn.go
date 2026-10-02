// Package metrics exposes OVN database row counts as Prometheus series.
package metrics

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// OtherNetwork is the network label value that aggregates folded networks.
const OtherNetwork = "_other"

// NetworkLabelMode controls how the per-network label is emitted.
type NetworkLabelMode struct {
	Off  bool
	TopN int
}

// ParseNetworkLabelMode parses "off" or "topN:<n>" with n >= 1.
func ParseNetworkLabelMode(s string) (NetworkLabelMode, error) {
	if s == "off" {
		return NetworkLabelMode{Off: true}, nil
	}
	if v, ok := strings.CutPrefix(s, "topN:"); ok {
		n, err := strconv.Atoi(v)
		if err == nil && n >= 1 {
			return NetworkLabelMode{TopN: n}, nil
		}
	}
	return NetworkLabelMode{}, fmt.Errorf("invalid per-network-labels %q: want \"off\" or \"topN:<n>\" with n >= 1", s)
}

// FoldTopN keeps the n largest networks (ties broken by name ascending) and
// sums the rest under "_other", which is omitted when zero.
func FoldTopN(counts map[string]int, n int) map[string]int {
	if len(counts) <= n {
		out := make(map[string]int, len(counts))
		for k, v := range counts {
			out[k] = v
		}
		return out
	}
	names := make([]string, 0, len(counts))
	for k := range counts {
		names = append(names, k)
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := names[i], names[j]
		if counts[a] != counts[b] {
			return counts[a] > counts[b]
		}
		return a < b
	})
	out := make(map[string]int, n+1)
	other := 0
	for i, k := range names {
		if i < n {
			out[k] = counts[k]
		} else {
			other += counts[k]
		}
	}
	if other > 0 {
		out[OtherNetwork] = other
	}
	return out
}
