package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

type fakeDrift struct{ missing, withPorts int }

func (f fakeDrift) Missing() (int, int) { return f.missing, f.withPorts }

func TestDriftCollectorEmits(t *testing.T) {
	nb, sb := &DBState{}, &DBState{}
	nb.Set(true, 0)
	sb.Set(true, 0)
	c := NewDriftCollector(fakeDrift{3, 40}, nb, sb)
	want := `# HELP ovnkube_controller_port_group_sb_missing Northbound Port_Groups holding ports that are absent from the southbound database.
# TYPE ovnkube_controller_port_group_sb_missing gauge
ovnkube_controller_port_group_sb_missing 3
# HELP ovnkube_controller_port_group_with_ports Northbound Port_Groups holding at least one port.
# TYPE ovnkube_controller_port_group_with_ports gauge
ovnkube_controller_port_group_with_ports 40
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want)); err != nil {
		t.Fatal(err)
	}
}

func TestDriftCollectorSuppressedUntilBothSynced(t *testing.T) {
	for _, tc := range []struct{ nb, sb bool }{{true, false}, {false, true}, {false, false}} {
		nb, sb := &DBState{}, &DBState{}
		nb.Set(tc.nb, 0)
		sb.Set(tc.sb, 0)
		c := NewDriftCollector(fakeDrift{3, 40}, nb, sb)
		if n := testutil.CollectAndCount(c); n != 0 {
			t.Errorf("nb=%v sb=%v: got %d series, want 0", tc.nb, tc.sb, n)
		}
	}
}
