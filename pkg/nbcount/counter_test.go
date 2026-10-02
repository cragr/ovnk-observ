package nbcount

import "testing"

func u(b byte) [16]byte { var x [16]byte; x[0] = b; return x }

func TestCounterInsertSnapshot(t *testing.T) {
	c := NewCounter()
	a, b := Key{"NetworkPolicy", "default"}, Key{"none", "blue"}
	for i, k := range []Key{a, a, b} {
		if op := c.Upsert("ACL", u(byte(i+1)), k); op != "insert" {
			t.Fatalf("op=%s", op)
		}
	}
	s := c.Snapshot()
	if s["ACL"][a] != 2 || s["ACL"][b] != 1 || len(s["ACL"]) != 2 {
		t.Fatalf("snapshot %v", s)
	}
}

func TestCounterModifyMovesBucket(t *testing.T) {
	c := NewCounter()
	a, b := Key{"x", "a"}, Key{"x", "b"}
	c.Upsert("ACL", u(1), a)
	if op := c.Upsert("ACL", u(1), b); op != "modify" {
		t.Fatalf("op=%s", op)
	}
	s := c.Snapshot()["ACL"]
	if _, ok := s[a]; ok {
		t.Fatalf("A should be absent: %v", s)
	}
	if s[b] != 1 {
		t.Fatalf("B=%d", s[b])
	}
	// same-key modify keeps count
	if op := c.Upsert("ACL", u(1), b); op != "modify" || c.Snapshot()["ACL"][b] != 1 {
		t.Fatal("same-key modify")
	}
}

func TestCounterDeleteUnknownIsNoop(t *testing.T) {
	c := NewCounter()
	if c.Delete("ACL", u(9)) {
		t.Fatal("unknown delete returned true")
	}
	c.Upsert("ACL", u(1), Key{"x", "a"})
	if c.Delete("ACL", u(9)) || c.Delete("Nope", u(1)) {
		t.Fatal("unknown delete returned true")
	}
	if c.Snapshot()["ACL"][Key{"x", "a"}] != 1 {
		t.Fatal("changed")
	}
	if !c.Delete("ACL", u(1)) {
		t.Fatal("known delete false")
	}
	if len(c.Snapshot()["ACL"]) != 0 {
		t.Fatalf("%v", c.Snapshot())
	}
	if c.Delete("ACL", u(1)) {
		t.Fatal("double delete true")
	}
}

func TestCounterResetKeepsUpdates(t *testing.T) {
	c := NewCounter()
	c.Upsert("ACL", u(1), Key{"x", "a"})
	c.RecordUpdate("ACL", "insert")
	c.Reset()
	if len(c.Snapshot()) != 0 {
		t.Fatalf("%v", c.Snapshot())
	}
	if c.Updates()[[2]string{"ACL", "insert"}] != 1 {
		t.Fatal("updates lost")
	}
	if op := c.Upsert("ACL", u(1), Key{"x", "a"}); op != "insert" {
		t.Fatal("after reset should insert")
	}
}
