package ovsdbmon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/cragr/ovnk-observ/pkg/nbcount"
)

// Keep test output pristine: the client logs every disconnect.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

// ---- fake ovsdb-server -----------------------------------------------------

type fakeServer struct {
	ln    net.Listener
	conns chan *fakeConn

	mu   sync.Mutex
	recv []map[string]json.RawMessage // every message any client sent
}

type fakeConn struct {
	c    net.Conn
	wmu  sync.Mutex
	msgs chan map[string]json.RawMessage
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	// Short dir: unix socket paths are limited to ~104 bytes on darwin.
	dir, err := os.MkdirTemp("", "ovsm")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", filepath.Join(dir, "db.sock"))
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeServer{ln: ln, conns: make(chan *fakeConn, 8)}
	t.Cleanup(func() { ln.Close(); os.RemoveAll(dir) })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			fc := &fakeConn{c: c, msgs: make(chan map[string]json.RawMessage, 64)}
			go s.readLoop(fc)
			s.conns <- fc
		}
	}()
	return s
}

func (s *fakeServer) readLoop(fc *fakeConn) {
	dec := json.NewDecoder(fc.c)
	defer close(fc.msgs)
	for {
		var m map[string]json.RawMessage
		if err := dec.Decode(&m); err != nil {
			return
		}
		s.mu.Lock()
		s.recv = append(s.recv, m)
		s.mu.Unlock()
		fc.msgs <- m
	}
}

func (s *fakeServer) path() string { return s.ln.Addr().String() }

func (s *fakeServer) accept(t *testing.T, within time.Duration) *fakeConn {
	t.Helper()
	select {
	case fc := <-s.conns:
		t.Cleanup(func() { fc.c.Close() })
		return fc
	case <-time.After(within):
		t.Fatalf("no connection within %v", within)
		return nil
	}
}

func (s *fakeServer) received() []map[string]json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]json.RawMessage(nil), s.recv...)
}

func (fc *fakeConn) next(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	select {
	case m, ok := <-fc.msgs:
		if !ok {
			t.Fatal("client connection closed")
		}
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for client message")
		return nil
	}
}

func (fc *fakeConn) write(t *testing.T, s string) {
	t.Helper()
	fc.wmu.Lock()
	defer fc.wmu.Unlock()
	if _, err := fc.c.Write([]byte(s)); err != nil {
		t.Fatalf("server write: %v", err)
	}
}

// expectMonitor reads the monitor request and checks it against the spec.
func (fc *fakeConn) expectMonitor(t *testing.T, db string, tables []TableSpec) {
	t.Helper()
	m := fc.next(t)
	var method string
	json.Unmarshal(m["method"], &method)
	if method != "monitor" || string(m["id"]) != "1" {
		t.Fatalf("want monitor id 1, got %v", stringify(m))
	}
	var params []json.RawMessage
	if err := json.Unmarshal(m["params"], &params); err != nil || len(params) != 3 {
		t.Fatalf("bad params %s", m["params"])
	}
	var gotDB, gotID string
	json.Unmarshal(params[0], &gotDB)
	json.Unmarshal(params[1], &gotID)
	if gotDB != db || gotID != "ovnk-observ" {
		t.Fatalf("params db=%q id=%q", gotDB, gotID)
	}
	var req map[string]struct{ Columns []string }
	if err := json.Unmarshal(params[2], &req); err != nil {
		t.Fatal(err)
	}
	if len(req) != len(tables) {
		t.Fatalf("monitor requests %d tables, want %d", len(req), len(tables))
	}
	for _, ts := range tables {
		r, ok := req[ts.Name]
		if !ok || len(r.Columns) != 1 || r.Columns[0] != ts.Column {
			t.Fatalf("table %s: got %+v", ts.Name, r)
		}
	}
}

// barrier sends an echo and waits for its reply; since the client processes
// messages in order, everything written before it has been applied.
func (fc *fakeConn) barrier(t *testing.T, tag string) {
	t.Helper()
	fc.write(t, fmt.Sprintf(`{"method":"echo","params":[%q],"id":%q}`, tag, tag))
	for {
		m := fc.next(t)
		if string(m["id"]) == fmt.Sprintf("%q", tag) {
			return
		}
	}
}

func stringify(m map[string]json.RawMessage) string {
	b, _ := json.Marshal(m)
	return string(b)
}

// ---- client harness --------------------------------------------------------

type stateEv struct {
	connected bool
	d         time.Duration
}

type harness struct {
	counter *nbcount.Counter
	states  chan stateEv
	cancel  context.CancelFunc
	done    chan error
}

var aclOnly = []TableSpec{{Name: "ACL", Column: "external_ids", KeyFromRow: true}}

func startClient(t *testing.T, srv *fakeServer, tables []TableSpec) *harness {
	t.Helper()
	h := &harness{counter: nbcount.NewCounter(), states: make(chan stateEv, 16), done: make(chan error, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	cfg := Config{
		Socket: srv.path(), Database: "OVN_Northbound", Tables: tables, Counter: h.counter,
		OnState: func(c bool, d time.Duration) { h.states <- stateEv{c, d} },
	}
	go func() { h.done <- Run(ctx, cfg) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-h.done:
			if err != context.Canceled {
				t.Errorf("Run returned %v, want context.Canceled", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})
	return h
}

func (h *harness) state(t *testing.T) stateEv {
	t.Helper()
	return h.stateWithin(t, 10*time.Second)
}

func (h *harness) stateWithin(t *testing.T, d time.Duration) stateEv {
	t.Helper()
	select {
	case s := <-h.states:
		return s
	case <-time.After(d):
		t.Fatal("timed out waiting for OnState")
		return stateEv{}
	}
}

const (
	u1 = "00000000-0000-0000-0000-000000000001"
	u2 = "00000000-0000-0000-0000-000000000002"
	u3 = "00000000-0000-0000-0000-000000000003"
	u4 = "00000000-0000-0000-0000-000000000004"
)

const vlanIDs = `["map",[["k8s.ovn.org/owner-type","NetworkPolicy"],["k8s.ovn.org/owner-controller","repro-vlan3000-network-controller"]]]`

var (
	keyVlan = nbcount.Key{OwnerType: "NetworkPolicy", Network: "repro-vlan3000"}
	keyNone = nbcount.Key{OwnerType: "none", Network: "default"}
)

func threeACLDump() string {
	return `{"id":1,"error":null,"result":{"ACL":{` +
		`"` + u1 + `":{"new":{"external_ids":` + vlanIDs + `}},` +
		`"` + u2 + `":{"new":{"external_ids":` + vlanIDs + `}},` +
		`"` + u3 + `":{"new":{"external_ids":["map",[]]}}}}}`
}

// ---- tests -----------------------------------------------------------------

func TestMonitorInitialDumpCounts(t *testing.T) {
	srv := newFakeServer(t)
	h := startClient(t, srv, aclOnly)
	fc := srv.accept(t, 3*time.Second)
	fc.expectMonitor(t, "OVN_Northbound", aclOnly)
	fc.write(t, threeACLDump())

	st := h.state(t)
	if !st.connected || st.d <= 0 {
		t.Fatalf("OnState = %+v, want (true, >0)", st)
	}
	s := h.counter.Snapshot()["ACL"]
	if s[keyVlan] != 2 || s[keyNone] != 1 || len(s) != 2 {
		t.Fatalf("snapshot %v", s)
	}
	if u := h.counter.Updates(); len(u) != 0 {
		t.Fatalf("initial dump recorded updates: %v", u)
	}
}

func TestMonitorAppliesUpdates(t *testing.T) {
	srv := newFakeServer(t)
	h := startClient(t, srv, aclOnly)
	fc := srv.accept(t, 3*time.Second)
	fc.expectMonitor(t, "OVN_Northbound", aclOnly)
	fc.write(t, threeACLDump())
	h.state(t)

	// insert u4 (vlan), modify u1 vlan->none, delete u2
	fc.write(t, `{"method":"update","params":["ovnk-observ",{"ACL":{`+
		`"`+u4+`":{"new":{"external_ids":`+vlanIDs+`}},`+
		`"`+u1+`":{"old":{"external_ids":`+vlanIDs+`},"new":{"external_ids":["map",[]]}},`+
		`"`+u2+`":{"old":{"external_ids":`+vlanIDs+`}}}}],"id":null}`)
	fc.barrier(t, "sync")

	s := h.counter.Snapshot()["ACL"]
	if s[keyVlan] != 1 || s[keyNone] != 2 || len(s) != 2 {
		t.Fatalf("snapshot %v", s)
	}
	u := h.counter.Updates()
	want := map[[2]string]uint64{{"ACL", "insert"}: 1, {"ACL", "modify"}: 1, {"ACL", "delete"}: 1}
	if len(u) != len(want) {
		t.Fatalf("updates %v", u)
	}
	for k, v := range want {
		if u[k] != v {
			t.Fatalf("updates %v, want %v", u, want)
		}
	}
}

func TestMonitorParamsBeforeMethod(t *testing.T) {
	// Key order in a JSON object is not guaranteed; params may precede method.
	srv := newFakeServer(t)
	h := startClient(t, srv, aclOnly)
	fc := srv.accept(t, 3*time.Second)
	fc.expectMonitor(t, "OVN_Northbound", aclOnly)
	fc.write(t, `{"result":{},"id":1,"error":null}`)
	h.state(t)
	fc.write(t, `{"params":["ovnk-observ",{"ACL":{"`+u1+`":{"new":{"external_ids":`+vlanIDs+`}}}}],"id":null,"method":"update"}`)
	fc.barrier(t, "sync")
	if s := h.counter.Snapshot()["ACL"]; s[keyVlan] != 1 {
		t.Fatalf("snapshot %v", s)
	}
}

func TestMonitorSkipsBadUUIDAndNonMapColumn(t *testing.T) {
	srv := newFakeServer(t)
	h := startClient(t, srv, aclOnly)
	fc := srv.accept(t, 3*time.Second)
	fc.expectMonitor(t, "OVN_Northbound", aclOnly)
	fc.write(t, `{"id":1,"error":null,"result":{"ACL":{`+
		`"not-a-uuid":{"new":{"external_ids":`+vlanIDs+`}},`+
		`"`+u1+`":{"new":{"external_ids":42}},`+
		`"`+u2+`":{"new":{"external_ids":["map",[["k8s.ovn.org/owner-type",7],["k8s.ovn.org/owner-controller","x-network-controller"]]]}},`+
		`"`+u3+`":{"new":{}}}}}`)
	if st := h.state(t); !st.connected {
		t.Fatalf("state %+v", st)
	}
	s := h.counter.Snapshot()["ACL"]
	if s[keyNone] != 2 || s[nbcount.Key{OwnerType: "none", Network: "x"}] != 1 || len(s) != 2 {
		t.Fatalf("snapshot %v", s)
	}
}

func TestMonitorConstantKeyTables(t *testing.T) {
	srv := newFakeServer(t)
	tables := []TableSpec{{Name: "Logical_Flow", Column: "table_id"}}
	h := startClient(t, srv, tables)
	fc := srv.accept(t, 3*time.Second)
	fc.expectMonitor(t, "OVN_Northbound", tables)
	fc.write(t, `{"id":1,"error":null,"result":{"Logical_Flow":{"`+u1+`":{"new":{"table_id":3}},"`+u2+`":{"new":{"table_id":4}}}}}`)
	h.state(t)
	if s := h.counter.Snapshot()["Logical_Flow"]; s[keyNone] != 2 || len(s) != 1 {
		t.Fatalf("snapshot %v", s)
	}
}

func TestMonitorRepliesToEcho(t *testing.T) {
	srv := newFakeServer(t)
	h := startClient(t, srv, aclOnly)
	fc := srv.accept(t, 3*time.Second)
	fc.expectMonitor(t, "OVN_Northbound", aclOnly)
	fc.write(t, threeACLDump())
	h.state(t)

	fc.write(t, `{"method":"echo","params":["x"],"id":"echo"}`)
	select {
	case m := <-fc.msgs:
		if string(m["id"]) != `"echo"` || string(m["result"]) != `["x"]` || string(m["error"]) != "null" {
			t.Fatalf("echo reply %s", stringify(m))
		}
		if _, ok := m["method"]; ok {
			t.Fatalf("echo reply has method: %s", stringify(m))
		}
	case <-time.After(time.Second):
		t.Fatal("no echo reply within 1s")
	}
}

func TestMonitorStreamsLargeDumpWithinMemory(t *testing.T) {
	const rows = 500_000
	srv := newFakeServer(t)
	h := startClient(t, srv, aclOnly)
	fc := srv.accept(t, 3*time.Second)
	fc.expectMonitor(t, "OVN_Northbound", aclOnly)

	// Sample peak HeapInuse during the dump: a decoder that materialized the
	// whole result and freed it afterwards would pass the post-GC check alone.
	stopSampling := make(chan struct{})
	peakCh := make(chan uint64, 1)
	go func() {
		var peak uint64
		var ms runtime.MemStats
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			runtime.ReadMemStats(&ms)
			peak = max(peak, ms.HeapInuse)
			select {
			case <-stopSampling:
				peakCh <- peak
				return
			case <-tick.C:
			}
		}
	}()

	werr := make(chan error, 1)
	go func() {
		w := bufio.NewWriterSize(fc.c, 64<<10)
		w.WriteString(`{"id":1,"error":null,"result":{"ACL":{`)
		var row []byte
		for i := 0; i < rows; i++ {
			if i > 0 {
				w.WriteByte(',')
			}
			nw := strconv.Itoa(3000 + i%10)
			row = append(row[:0], '"')
			row = appendHex(row, uint64(i), 8)
			row = append(row, "-0000-4000-8000-"...)
			row = appendHex(row, uint64(i), 12)
			row = append(row, `":{"new":{"external_ids":["map",[["k8s.ovn.org/id","repro-vlan`...)
			row = append(row, nw...)
			row = append(row, `-network-controller:NetworkPolicy:ns:np:Ingress:`...)
			row = strconv.AppendInt(row, int64(i), 10)
			row = append(row, `"],["k8s.ovn.org/name","ns:np`...)
			row = strconv.AppendInt(row, int64(i), 10)
			row = append(row, `"],["k8s.ovn.org/owner-controller","repro-vlan`...)
			row = append(row, nw...)
			row = append(row, `-network-controller"],["k8s.ovn.org/owner-type","NetworkPolicy"]]]}}`...)
			w.Write(row)
		}
		fmt.Fprint(w, `}}}`)
		werr <- w.Flush()
	}()

	// ~1.5s normally; encoding/json's byte scanner is ~10x slower under -race.
	if st := h.stateWithin(t, 2*time.Minute); !st.connected {
		t.Fatalf("state %+v", st)
	}
	close(stopSampling)
	peak := <-peakCh
	if err := <-werr; err != nil {
		t.Fatal(err)
	}
	t.Logf("peak HeapInuse during %d-row dump: %.1f MiB", rows, float64(peak)/(1<<20))
	if peak >= 128<<20 {
		t.Fatalf("peak HeapInuse %d >= 128 MiB", peak)
	}
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	t.Logf("HeapAlloc after %d-row dump: %.1f MiB", rows, float64(ms.HeapAlloc)/(1<<20))
	if ms.HeapAlloc >= 96<<20 {
		t.Fatalf("HeapAlloc %d >= 96 MiB", ms.HeapAlloc)
	}
	total := 0
	for _, n := range h.counter.Snapshot()["ACL"] {
		total += n
	}
	if total != rows {
		t.Fatalf("total %d, want %d", total, rows)
	}
}

func appendHex(b []byte, v uint64, width int) []byte {
	const digits = "0123456789abcdef"
	for i := width - 1; i >= 0; i-- {
		b = append(b, digits[(v>>(4*uint(i)))&0xf])
	}
	return b
}

func TestMonitorReplyErrorReconnects(t *testing.T) {
	setBackoff(t, 10*time.Millisecond)
	srv := newFakeServer(t)
	h := startClient(t, srv, aclOnly)
	fc := srv.accept(t, 3*time.Second)
	fc.expectMonitor(t, "OVN_Northbound", aclOnly)
	fc.write(t, `{"id":1,"result":null,"error":{"error":"unknown database"}}`)
	if st := h.state(t); st.connected {
		t.Fatalf("state %+v", st)
	}
	srv.accept(t, 3*time.Second)
}

func setBackoff(t *testing.T, d time.Duration) {
	old := testBackoffStart
	testBackoffStart = d
	t.Cleanup(func() { testBackoffStart = old })
}

func TestRunReconnectsAfterServerClose(t *testing.T) {
	setBackoff(t, 10*time.Millisecond)
	srv := newFakeServer(t)
	h := startClient(t, srv, aclOnly)
	fc := srv.accept(t, 3*time.Second)
	fc.expectMonitor(t, "OVN_Northbound", aclOnly)
	fc.write(t, threeACLDump())
	if st := h.state(t); !st.connected {
		t.Fatalf("state %+v", st)
	}
	fc.c.Close()
	if st := h.state(t); st.connected || st.d != 0 {
		t.Fatalf("OnState = %+v, want (false, 0)", st)
	}
	for tbl, m := range h.counter.Snapshot() {
		if len(m) != 0 {
			t.Fatalf("snapshot not empty: %s %v", tbl, m)
		}
	}
	fc2 := srv.accept(t, 3*time.Second)
	fc2.expectMonitor(t, "OVN_Northbound", aclOnly)
}

func TestRunReturnsOnCancelWhileDisconnected(t *testing.T) {
	setBackoff(t, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{Socket: "/nonexistent/ovsm.sock", Database: "x", Tables: aclOnly, Counter: nbcount.NewCounter()})
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("Run = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return")
	}
}

func TestNextBackoff(t *testing.T) {
	start := time.Second
	cases := []struct {
		cur, lasted, wantWait, wantNext time.Duration
	}{
		{time.Second, 0, time.Second, 2 * time.Second},
		{4 * time.Second, 10 * time.Second, 4 * time.Second, 8 * time.Second},
		{4 * time.Minute, 0, 4 * time.Minute, 5 * time.Minute},
		{5 * time.Minute, 0, 5 * time.Minute, 5 * time.Minute},
		{5 * time.Minute, time.Minute, time.Second, 2 * time.Second},
	}
	for _, c := range cases {
		w, n := nextBackoff(c.cur, start, c.lasted)
		if w != c.wantWait || n != c.wantNext {
			t.Errorf("nextBackoff(%v,%v) = (%v,%v), want (%v,%v)", c.cur, c.lasted, w, n, c.wantWait, c.wantNext)
		}
	}
}

func TestMonitorNeverSendsTransact(t *testing.T) {
	setBackoff(t, 10*time.Millisecond)
	srv := newFakeServer(t)
	h := startClient(t, srv, NBTables)
	fc := srv.accept(t, 3*time.Second)
	fc.expectMonitor(t, "OVN_Northbound", NBTables)
	fc.write(t, threeACLDump())
	h.state(t)
	fc.write(t, `{"method":"update","params":["ovnk-observ",{"ACL":{"`+u4+`":{"new":{"external_ids":`+vlanIDs+`}}}}],"id":null}`)
	fc.barrier(t, "e1")
	fc.c.Close()
	h.state(t)
	fc2 := srv.accept(t, 3*time.Second)
	fc2.expectMonitor(t, "OVN_Northbound", NBTables)

	monitors, replies := 0, 0
	for _, m := range srv.received() {
		var method string
		if raw, ok := m["method"]; ok {
			json.Unmarshal(raw, &method)
		}
		switch {
		case method == "monitor":
			monitors++
		case method == "" && m["result"] != nil:
			replies++
		default:
			t.Fatalf("client sent forbidden message: %s", stringify(m))
		}
	}
	if monitors != 2 || replies != 1 {
		t.Fatalf("monitors=%d replies=%d", monitors, replies)
	}
}

func TestTableSpecs(t *testing.T) {
	nb := []TableSpec{}
	for _, n := range []string{"ACL", "Port_Group", "Address_Set", "Logical_Switch_Port", "Logical_Switch", "Logical_Router", "Logical_Router_Port", "Load_Balancer"} {
		nb = append(nb, TableSpec{Name: n, Column: "external_ids", KeyFromRow: true})
	}
	// NB schema 7.18.0: Load_Balancer_Group has no external_ids column;
	// monitoring it there makes ovsdb-server reject the whole monitor request.
	nb = append(nb, TableSpec{Name: "Load_Balancer_Group", Column: "name"})
	if len(NBTables) != len(nb) {
		t.Fatalf("NBTables %v", NBTables)
	}
	for i := range nb {
		if NBTables[i] != nb[i] {
			t.Fatalf("NBTables[%d] = %+v, want %+v", i, NBTables[i], nb[i])
		}
	}
	sb := []TableSpec{
		{Name: "Logical_Flow", Column: "table_id"},
		{Name: "Port_Binding", Column: "logical_port"},
		{Name: "Datapath_Binding", Column: "tunnel_key"},
		{Name: "MAC_Binding", Column: "logical_port"},
		{Name: "FDB", Column: "dp_key"},
	}
	if len(SBTables) != len(sb) {
		t.Fatalf("SBTables %v", SBTables)
	}
	for i := range sb {
		if SBTables[i] != sb[i] {
			t.Fatalf("SBTables[%d] = %+v", i, SBTables[i])
		}
	}
}

func TestParseMap(t *testing.T) {
	cases := []struct {
		in   string
		want map[string]string
	}{
		{`["map",[]]`, map[string]string{}},
		{` [ "map" , [ [ "a" , "b" ] , ["c","d"] ] ] `, map[string]string{"a": "b", "c": "d"}},
		{`["map",[["k\"q","vé\n"]]]`, map[string]string{`k"q`: "vé\n"}},
		{`["map",[["a",7],["b","x"]]]`, map[string]string{"b": "x"}},
		{`["map",[["a","b","c"],["d","e"]]]`, map[string]string{"d": "e"}},
		{`["set",[]]`, nil},
		{`42`, nil},
		{`null`, nil},
		{``, nil},
		{`["map",[["a","b"]]`, nil},
		{`["map",[["a","b"]]] x`, nil},
		{`"map"`, nil},
	}
	for i, c := range cases {
		fast, ok := parseMapFast([]byte(c.in))
		if slow := parseMapSlow(json.RawMessage(c.in)); ok && !reflect.DeepEqual(fast, slow) {
			t.Errorf("parseMapFast(%s) = %#v, parseMapSlow = %#v", c.in, fast, slow)
		}
		if i < 2 && !ok { // plain string maps must take the fast path
			t.Errorf("parseMapFast(%s) not handled", c.in)
		}
		got := parseMap(json.RawMessage(c.in))
		if len(got) != len(c.want) { // nil and empty are equivalent to callers
			t.Errorf("parseMap(%s) = %#v, want %#v", c.in, got, c.want)
			continue
		}
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("parseMap(%s) = %#v, want %#v", c.in, got, c.want)
			}
		}
	}
}

// runOnce must report uptime measured from initial sync, never from dial: a
// connection that dies mid-dump (however long it lasted) must not reset
// backoff, or a ~1 GB dump that fails after >1m is re-requested every minute.
func TestRunOnceUptimeCountsFromSync(t *testing.T) {
	srv := newFakeServer(t)
	cfg := Config{Socket: srv.path(), Database: "OVN_Northbound", Tables: aclOnly, Counter: nbcount.NewCounter()}
	log := slog.Default()

	// Never synced: server holds the connection 100ms, sends a partial dump, closes.
	res := make(chan time.Duration, 1)
	go func() { up, _ := runOnce(context.Background(), cfg, log); res <- up }()
	fc := srv.accept(t, 3*time.Second)
	fc.expectMonitor(t, "OVN_Northbound", aclOnly)
	fc.write(t, `{"id":1,"error":null,"result":{"ACL":{"`+u1+`":{"new":{}}`)
	time.Sleep(100 * time.Millisecond)
	fc.c.Close()
	if up := <-res; up != 0 {
		t.Fatalf("unsynced connection reported uptime %v, want 0", up)
	}
	if w, _ := nextBackoff(4*time.Minute, time.Second, 0); w != 4*time.Minute {
		t.Fatalf("unsynced connection reset backoff: wait %v", w)
	}

	// Synced: uptime counts from the reply, not from dial.
	go func() { up, _ := runOnce(context.Background(), cfg, log); res <- up }()
	fc = srv.accept(t, 3*time.Second)
	fc.expectMonitor(t, "OVN_Northbound", aclOnly)
	time.Sleep(time.Second) // slow "dump": must not count
	fc.write(t, threeACLDump())
	fc.barrier(t, "synced")
	time.Sleep(50 * time.Millisecond)
	fc.c.Close()
	up := <-res
	if up < 50*time.Millisecond || up >= time.Second {
		t.Fatalf("synced uptime %v, want ~50ms measured from sync", up)
	}
}
