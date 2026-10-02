package metrics

import (
	"fmt"
	"strings"
	"testing"

	"github.com/cragr/ovnk-observ/pkg/nbcount"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func uid(i int) [16]byte {
	var x [16]byte
	x[0], x[1] = byte(i), byte(i>>8)
	return x
}

func TestNodeCollectorEmitsObjects(t *testing.T) {
	nb, sb := nbcount.NewCounter(), nbcount.NewCounter()
	nb.Upsert("ACL", uid(1), nbcount.Key{OwnerType: "NetworkPolicy", Network: "net1"})
	nb.Upsert("ACL", uid(2), nbcount.Key{OwnerType: "NetworkPolicy", Network: "net1"})
	nb.Upsert("ACL", uid(3), nbcount.Key{OwnerType: "none", Network: "default"})
	for i := 0; i < 4; i++ {
		sb.Upsert("Logical_Flow", uid(i), nbcount.Key{OwnerType: "none", Network: "default"})
	}
	nbS, sbS := &DBState{}, &DBState{}
	nbS.Set(true, 0)
	c := NewNodeCollector(nb, sb, nbS, sbS, NetworkLabelMode{TopN: 50})
	exp := `
# HELP ovnk_observ_db_connected Whether the exporter is connected to the OVN database.
# TYPE ovnk_observ_db_connected gauge
ovnk_observ_db_connected{db="nb"} 1
ovnk_observ_db_connected{db="sb"} 0
# HELP ovnkube_controller_nb_db_objects Number of rows in the OVN northbound database.
# TYPE ovnkube_controller_nb_db_objects gauge
ovnkube_controller_nb_db_objects{network="default",owner_type="none",table="ACL"} 1
ovnkube_controller_nb_db_objects{network="net1",owner_type="NetworkPolicy",table="ACL"} 2
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(exp),
		"ovnk_observ_db_connected", "ovnkube_controller_nb_db_objects"); err != nil {
		t.Fatal(err)
	}
	// SB disconnected: no sb objects. Connect and check.
	sbS.Set(true, 0)
	exp = `
# HELP ovnkube_controller_sb_db_objects Number of rows in the OVN southbound database.
# TYPE ovnkube_controller_sb_db_objects gauge
ovnkube_controller_sb_db_objects{table="Logical_Flow"} 4
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(exp), "ovnkube_controller_sb_db_objects"); err != nil {
		t.Fatal(err)
	}
}

func TestNodeCollectorTopNFoldsPerTable(t *testing.T) {
	nb := nbcount.NewCounter()
	total := 0
	for n := 0; n < 60; n++ {
		for j := 0; j <= n; j++ {
			nb.Upsert("ACL", uid(total), nbcount.Key{OwnerType: "NetworkPolicy", Network: fmt.Sprintf("net%02d", n)})
			total++
		}
	}
	nbS := &DBState{}
	nbS.Set(true, 0)
	c := NewNodeCollector(nb, nbcount.NewCounter(), nbS, &DBState{}, NetworkLabelMode{TopN: 50})
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(c)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	nets, sum := map[string]bool{}, 0.0
	for _, mf := range mfs {
		if mf.GetName() != "ovnkube_controller_nb_db_objects" {
			continue
		}
		for _, m := range mf.Metric {
			lbl := map[string]string{}
			for _, l := range m.Label {
				lbl[l.GetName()] = l.GetValue()
			}
			if lbl["table"] == "ACL" {
				nets[lbl["network"]] = true
				sum += m.Gauge.GetValue()
			}
		}
	}
	if len(nets) != 51 || !nets["_other"] {
		t.Errorf("got %d networks, want 51 incl _other", len(nets))
	}
	if int(sum) != total {
		t.Errorf("sum %v want %d", sum, total)
	}
}

func TestNodeCollectorOffMode(t *testing.T) {
	nb := nbcount.NewCounter()
	nb.Upsert("ACL", uid(1), nbcount.Key{OwnerType: "NetworkPolicy", Network: "a"})
	nb.Upsert("ACL", uid(2), nbcount.Key{OwnerType: "NetworkPolicy", Network: "b"})
	nbS := &DBState{}
	nbS.Set(true, 0)
	c := NewNodeCollector(nb, nbcount.NewCounter(), nbS, &DBState{}, NetworkLabelMode{Off: true})
	exp := `
# HELP ovnkube_controller_nb_db_objects Number of rows in the OVN northbound database.
# TYPE ovnkube_controller_nb_db_objects gauge
ovnkube_controller_nb_db_objects{network="",owner_type="NetworkPolicy",table="ACL"} 2
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(exp), "ovnkube_controller_nb_db_objects"); err != nil {
		t.Fatal(err)
	}
}

func TestCollectorOmitsObjectsWhenDisconnected(t *testing.T) {
	nb := nbcount.NewCounter()
	nb.Upsert("ACL", uid(1), nbcount.Key{OwnerType: "none", Network: "default"})
	nbS := &DBState{}
	nbS.Set(false, 0)
	c := NewNodeCollector(nb, nbcount.NewCounter(), nbS, &DBState{}, NetworkLabelMode{TopN: 50})
	if n := testutil.CollectAndCount(c, "ovnkube_controller_nb_db_objects"); n != 0 {
		t.Errorf("got %d series, want 0", n)
	}
	exp := `
# HELP ovnk_observ_db_connected Whether the exporter is connected to the OVN database.
# TYPE ovnk_observ_db_connected gauge
ovnk_observ_db_connected{db="nb"} 0
ovnk_observ_db_connected{db="sb"} 0
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(exp), "ovnk_observ_db_connected"); err != nil {
		t.Fatal(err)
	}
}

func TestNodeCollectorInitialSync(t *testing.T) {
	nbS := &DBState{}
	c := NewNodeCollector(nbcount.NewCounter(), nbcount.NewCounter(), nbS, &DBState{}, NetworkLabelMode{TopN: 5})
	if n := testutil.CollectAndCount(c, "ovnk_observ_initial_sync_seconds"); n != 0 {
		t.Fatalf("got %d before sync", n)
	}
	nbS.Set(true, 1500*1e6)
	nbS.Set(false, 0)
	exp := `
# HELP ovnk_observ_initial_sync_seconds Duration of the last initial database sync in seconds.
# TYPE ovnk_observ_initial_sync_seconds gauge
ovnk_observ_initial_sync_seconds{db="nb"} 1.5
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(exp), "ovnk_observ_initial_sync_seconds"); err != nil {
		t.Fatal(err)
	}
}

func TestNodeCollectorUpdatesCounter(t *testing.T) {
	nb := nbcount.NewCounter()
	nb.RecordUpdate("ACL", "delete")
	nb.RecordUpdate("ACL", "delete")
	c := NewNodeCollector(nb, nbcount.NewCounter(), &DBState{}, &DBState{}, NetworkLabelMode{TopN: 50})
	exp := `
# HELP ovnkube_controller_nb_db_updates_total Total number of northbound database row updates seen.
# TYPE ovnkube_controller_nb_db_updates_total counter
ovnkube_controller_nb_db_updates_total{op="delete",table="ACL"} 2
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(exp), "ovnkube_controller_nb_db_updates_total"); err != nil {
		t.Fatal(err)
	}
}
