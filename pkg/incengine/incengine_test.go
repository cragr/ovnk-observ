package incengine

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseIncidentFixture(t *testing.T) {
	m := Parse(readFixture(t, "show-stats-incident.txt"))
	if got, want := m["northd"], (Stats{167, 716840, 0}); got != want {
		t.Errorf("northd = %+v, want %+v", got, want)
	}
	if got, want := m["lflow"], (Stats{13320, 402740, 0}); got != want {
		t.Errorf("lflow = %+v, want %+v", got, want)
	}
	if m["NB_acl"].Compute != 0 {
		t.Errorf("NB_acl compute = %d, want 0", m["NB_acl"].Compute)
	}
}

func TestParseLabFixture(t *testing.T) {
	b, err := os.ReadFile("testdata/show-stats-lab.txt")
	if err != nil {
		t.Fatal(err)
	}
	m := Parse(string(b))
	if _, ok := m["northd"]; !ok {
		t.Fatal("no northd key")
	}
	for _, n := range DefaultNodes {
		if s, ok := m[n]; ok && s.Recompute+s.Compute == 0 {
			t.Errorf("%s: recompute+compute = 0", n)
		}
	}
}

func TestParseToleratesNoise(t *testing.T) {
	in := "\r\n\nNode: x\r\nNode: y\r\n- foo: 3\r\n- recompute:  abc\r\n\r\n- compute: 5\r\n"
	m := Parse(in)
	if got := m["x"]; got != (Stats{}) {
		t.Errorf("x = %+v, want zero", got)
	}
	if _, ok := m["x"]; !ok {
		t.Error("x missing")
	}
	if _, ok := m["foo"]; ok {
		t.Error("foo should be ignored")
	}
	if got := m["y"]; got != (Stats{Compute: 5}) {
		t.Errorf("y = %+v", got)
	}
}

func fixtureFetch(s string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return s, nil }
}

func TestCollectorAllowlist(t *testing.T) {
	c := NewCollector(fixtureFetch(readFixture(t, "show-stats-incident.txt")), []string{"northd", "not_a_node"}, time.Second)
	want := `
# HELP ovn_northd_inc_engine_runs_total Total northd incremental-engine node runs by type.
# TYPE ovn_northd_inc_engine_runs_total counter
ovn_northd_inc_engine_runs_total{engine_node="northd",type="cancel"} 0
ovn_northd_inc_engine_runs_total{engine_node="northd",type="compute"} 716840
ovn_northd_inc_engine_runs_total{engine_node="northd",type="recompute"} 167
# HELP ovnk_observ_appctl_errors_total Total failed ovn-appctl requests by command.
# TYPE ovnk_observ_appctl_errors_total counter
ovnk_observ_appctl_errors_total{command="inc-engine/show-stats"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(want)); err != nil {
		t.Fatal(err)
	}
}

func TestCollectorCachesOneScrape(t *testing.T) {
	fix := readFixture(t, "show-stats-incident.txt")
	calls := 0
	fetch := func(context.Context) (string, error) {
		calls++
		if calls == 1 {
			return fix, nil
		}
		return "", errors.New("boom")
	}
	c := NewCollector(fetch, []string{"northd"}, time.Second)
	const errHdr = "# HELP ovnk_observ_appctl_errors_total Total failed ovn-appctl requests by command.\n# TYPE ovnk_observ_appctl_errors_total counter\n"
	const runsHdr = "# HELP ovn_northd_inc_engine_runs_total Total northd incremental-engine node runs by type.\n# TYPE ovn_northd_inc_engine_runs_total counter\n"
	runs := `ovn_northd_inc_engine_runs_total{engine_node="northd",type="cancel"} 0
ovn_northd_inc_engine_runs_total{engine_node="northd",type="compute"} 716840
ovn_northd_inc_engine_runs_total{engine_node="northd",type="recompute"} 167
`
	errLine := func(n string) string {
		return `ovnk_observ_appctl_errors_total{command="inc-engine/show-stats"} ` + n + "\n"
	}
	steps := []string{
		runsHdr + runs + errHdr + errLine("0"),
		runsHdr + runs + errHdr + errLine("1"),
		errHdr + errLine("2"),
	}
	for i, want := range steps {
		if err := testutil.CollectAndCompare(c, strings.NewReader(want)); err != nil {
			t.Fatalf("scrape %d: %v", i+1, err)
		}
	}
}
