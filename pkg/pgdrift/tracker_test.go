package pgdrift

import (
	"encoding/json"
	"testing"
)

func uid(i int) [16]byte {
	var x [16]byte
	x[0], x[1] = byte(i), byte(i>>8)
	return x
}

const (
	withPorts = `["uuid","0b8d6c3e-7f1a-4c3e-9d2a-1a2b3c4d5e6f"]`
	noPorts   = `["set",[]]`
)

func pg(name, ports string) map[string]json.RawMessage {
	n, _ := json.Marshal(name)
	return map[string]json.RawMessage{"name": n, "ports": json.RawMessage(ports)}
}

func expect(t *testing.T, tr *Tracker, wantMissing, wantWith int) {
	t.Helper()
	m, w := tr.Missing()
	if m != wantMissing || w != wantWith {
		t.Fatalf("Missing() = (%d, %d), want (%d, %d)", m, w, wantMissing, wantWith)
	}
}

func TestHasPorts(t *testing.T) {
	for raw, want := range map[string]bool{
		`["set",[]]`: false, `["set", [ ]]`: false, ``: false,
		`["uuid","0b8d6c3e-7f1a-4c3e-9d2a-1a2b3c4d5e6f"]`:           true,
		`["set",[["uuid","0b8d6c3e-7f1a-4c3e-9d2a-1a2b3c4d5e6f"]]]`: true,
	} {
		if got := HasPorts(json.RawMessage(raw)); got != want {
			t.Errorf("HasPorts(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestSBBaseName(t *testing.T) {
	for in, want := range map[string]string{
		"12_pgA": "pgA", "pgA": "pgA", "12pgA": "12pgA", "_pgA": "_pgA",
	} {
		if got := SBBaseName(in); got != want {
			t.Errorf("SBBaseName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTrackerMissing(t *testing.T) {
	tr := NewTracker()
	tr.NB().Upsert("Port_Group", uid(1), pg("pgA", withPorts))
	tr.NB().Upsert("Port_Group", uid(2), pg("pgB", withPorts))
	tr.NB().Upsert("Port_Group", uid(3), pg("pgC", noPorts))
	tr.SB().Upsert("Port_Group", uid(11), pg("3_pgA", noPorts))
	tr.SB().Upsert("Port_Group", uid(12), pg("7_pgA", noPorts))
	expect(t, tr, 1, 2)
	tr.SB().Delete("Port_Group", uid(11))
	expect(t, tr, 1, 2)
	tr.SB().Delete("Port_Group", uid(12))
	expect(t, tr, 2, 2)
}

func TestTrackerPortsTransitions(t *testing.T) {
	tr := NewTracker()
	tr.NB().Upsert("Port_Group", uid(1), pg("pgB", withPorts))
	expect(t, tr, 1, 1)
	tr.NB().Upsert("Port_Group", uid(1), pg("pgB", noPorts))
	expect(t, tr, 0, 0)
	tr.NB().Upsert("Port_Group", uid(1), pg("pgB", withPorts))
	expect(t, tr, 1, 1)
}

func TestTrackerRename(t *testing.T) {
	tr := NewTracker()
	tr.NB().Upsert("Port_Group", uid(1), pg("pgA", withPorts))
	tr.SB().Upsert("Port_Group", uid(11), pg("1_pgA", noPorts))
	expect(t, tr, 0, 1)
	tr.NB().Upsert("Port_Group", uid(1), pg("pgZ", withPorts))
	expect(t, tr, 1, 1)
}

func TestTrackerSBRenameDecrementsOld(t *testing.T) {
	tr := NewTracker()
	tr.NB().Upsert("Port_Group", uid(1), pg("pgA", withPorts))
	tr.SB().Upsert("Port_Group", uid(11), pg("1_pgA", noPorts))
	tr.SB().Upsert("Port_Group", uid(11), pg("1_pgQ", noPorts))
	expect(t, tr, 1, 1)
}

func TestTrackerMissingNameColumn(t *testing.T) {
	tr := NewTracker()
	tr.NB().Upsert("Port_Group", uid(1), map[string]json.RawMessage{"ports": json.RawMessage(withPorts)})
	expect(t, tr, 1, 1)
}

func TestTrackerResetSide(t *testing.T) {
	tr := NewTracker()
	tr.NB().Upsert("Port_Group", uid(1), pg("pgA", withPorts))
	tr.SB().Upsert("Port_Group", uid(11), pg("1_pgA", noPorts))
	expect(t, tr, 0, 1)
	tr.SB().Reset()
	expect(t, tr, 1, 1)
	tr.NB().Reset()
	expect(t, tr, 0, 0)
}

func TestTrackerIgnoresOtherTables(t *testing.T) {
	tr := NewTracker()
	tr.NB().Upsert("ACL", uid(1), pg("pgA", withPorts))
	tr.SB().Upsert("ACL", uid(2), pg("1_pgA", noPorts))
	expect(t, tr, 0, 0)
	tr.NB().Upsert("Port_Group", uid(3), pg("pgA", withPorts))
	tr.SB().Delete("ACL", uid(2))
	expect(t, tr, 1, 1)
}
