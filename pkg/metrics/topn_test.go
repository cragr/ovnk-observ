package metrics

import (
	"reflect"
	"testing"
)

func TestParseNetworkLabelMode(t *testing.T) {
	ok := map[string]NetworkLabelMode{"topN:50": {TopN: 50}, "off": {Off: true}}
	for in, want := range ok {
		got, err := ParseNetworkLabelMode(in)
		if err != nil || got != want {
			t.Errorf("%q: got %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, in := range []string{"topN:0", "top:5", "", "topN:", "topN:-1", "topN:x"} {
		if _, err := ParseNetworkLabelMode(in); err == nil {
			t.Errorf("%q: expected error", in)
		}
	}
}

func TestFoldTopN(t *testing.T) {
	in := map[string]int{"a": 5, "b": 3, "c": 3, "d": 1}
	got := FoldTopN(in, 2)
	want := map[string]int{"a": 5, "b": 3, "_other": 4}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
	if got := FoldTopN(in, 10); !reflect.DeepEqual(got, in) {
		t.Errorf("n=10: got %v", got)
	}
}
