// Package ovsdbmon is a minimal, strictly read-only OVSDB (RFC 7047) monitor
// client. It sends a single "monitor" request per connection, answers "echo"
// requests, and streams the initial dump and later updates row by row into an
// nbcount.Counter. It never sends "transact" or any other request.
package ovsdbmon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/cragr/ovnk-observ/pkg/nbcount"
)

// TableSpec selects one table and the column monitored on it, plus any Extra
// columns requested after it. KeyFromRow derives the counter Key from the
// column's external_ids map; otherwise every row counts under
// Key{"none","default"}.
type TableSpec struct {
	Name       string
	Column     string
	KeyFromRow bool
	Extra      []string
}

// RowHook receives every applied row. Upsert gets the full monitor "new" map
// (all monitored columns), never the "old" delta. Delete is called for
// old-only, non-initial updates. Reset is called after every disconnect.
// Methods are called from the monitor goroutine.
type RowHook interface {
	Upsert(table string, uuid [16]byte, row map[string]json.RawMessage)
	Delete(table string, uuid [16]byte)
	Reset()
}

// WithColumns returns a copy of tables with cols monitored on table. If table
// is present, cols are appended to its Extra; otherwise a new TableSpec with
// Column cols[0] and Extra cols[1:] is appended. The input is not modified.
func WithColumns(tables []TableSpec, table string, cols ...string) []TableSpec {
	out := make([]TableSpec, len(tables), len(tables)+1)
	copy(out, tables)
	for i := range out {
		if out[i].Name == table {
			out[i].Extra = append(append([]string(nil), out[i].Extra...), cols...)
			return out
		}
	}
	if len(cols) == 0 {
		return out
	}
	return append(out, TableSpec{Name: table, Column: cols[0], Extra: append([]string(nil), cols[1:]...)})
}

// Config configures Run.
type Config struct {
	Socket, Database string
	Tables           []TableSpec
	Counter          *nbcount.Counter
	// Hook, if set, sees every applied row and is Reset after each disconnect.
	Hook RowHook
	// OnState, if set, is called with (true, initial-sync duration) once the
	// monitor reply has been fully applied, and with (false, 0) whenever a
	// connection attempt fails or an established connection ends.
	OnState func(connected bool, initialSync time.Duration)
}

func nbSpec(name string) TableSpec {
	return TableSpec{Name: name, Column: "external_ids", KeyFromRow: true}
}

// NBTables are the OVN_Northbound tables counted by owner type and network.
// Load_Balancer_Group has no external_ids column (NB schema 7.18.0), so its
// rows count under owner_type="none", network="default".
var NBTables = []TableSpec{
	nbSpec("ACL"), nbSpec("Port_Group"), nbSpec("Address_Set"),
	nbSpec("Logical_Switch_Port"), nbSpec("Logical_Switch"),
	nbSpec("Logical_Router"), nbSpec("Logical_Router_Port"),
	nbSpec("Load_Balancer"),
	{Name: "Load_Balancer_Group", Column: "name"},
}

// SBTables are the OVN_Southbound tables counted by table only. Each monitors
// one small column so the dump carries little more than row UUIDs.
var SBTables = []TableSpec{
	{Name: "Logical_Flow", Column: "table_id"},
	{Name: "Port_Binding", Column: "logical_port"},
	{Name: "Datapath_Binding", Column: "tunnel_key"},
	{Name: "MAC_Binding", Column: "logical_port"},
	{Name: "FDB", Column: "dp_key"},
}

const (
	monitorID           = "ovnk-observ"
	defaultBackoffStart = time.Second
	backoffCap          = 5 * time.Minute
	stableAfter         = time.Minute // up this long after sync resets backoff
	writeTimeout        = 10 * time.Second
	readBufSize         = 256 << 10 // json.Decoder alone issues many tiny reads
)

// testBackoffStart, when non-zero, replaces the 1s initial backoff (tests only).
var testBackoffStart time.Duration

// nextBackoff returns how long to wait now and the backoff to use next time,
// given the current backoff and how long the last connection stayed up after
// its initial sync (0 if it never synced).
func nextBackoff(cur, start, lasted time.Duration) (wait, next time.Duration) {
	if lasted >= stableAfter {
		cur = start
	}
	next = cur * 2
	if next > backoffCap {
		next = backoffCap
	}
	return cur, next
}

// Run connects, monitors and streams until ctx is cancelled, reconnecting with
// exponential backoff (1s, x2, cap 5m; reset to 1s only after a connection
// that stayed up >= 1m past its initial sync).
// After every disconnect OnState(false, 0) is called, then the counter (and
// Hook) are Reset.
// It returns ctx.Err() once ctx is done.
func Run(ctx context.Context, cfg Config) error {
	start := defaultBackoffStart
	if testBackoffStart > 0 {
		start = testBackoffStart
	}
	log := slog.Default().With("component", "ovsdbmon", "socket", cfg.Socket, "db", cfg.Database)
	backoff := start
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		lasted, err := runOnce(ctx, cfg, log)
		// Publish "disconnected" before emptying the state, so a scrape that
		// already read connected=true never sees a reset counter or hook.
		setState(cfg, false, 0)
		cfg.Counter.Reset()
		if cfg.Hook != nil {
			cfg.Hook.Reset()
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var wait time.Duration
		wait, backoff = nextBackoff(backoff, start, lasted)
		log.Warn("ovsdb monitor disconnected; reconnecting", "err", err, "up_since_sync", lasted, "retry_in", wait)
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

func setState(cfg Config, connected bool, d time.Duration) {
	if cfg.OnState != nil {
		cfg.OnState(connected, d)
	}
}

// conn serializes writes (the monitor request and echo replies).
type conn struct {
	c   net.Conn
	wmu sync.Mutex
}

func (c *conn) write(b []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.c.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return err
	}
	_, err := c.c.Write(b)
	return err
}

// echoReply answers {"method":"echo","params":P,"id":X} with the same P and X.
func (c *conn) echoReply(id, params json.RawMessage) error {
	b := make([]byte, 0, 32+len(id)+len(params))
	b = append(b, `{"id":`...)
	b = append(b, id...)
	b = append(b, `,"result":`...)
	b = append(b, params...)
	b = append(b, `,"error":null}`...)
	return c.write(b)
}

func monitorRequest(cfg Config) ([]byte, error) {
	reqs := make(map[string]any, len(cfg.Tables))
	for _, t := range cfg.Tables {
		reqs[t.Name] = map[string][]string{"columns": append([]string{t.Column}, t.Extra...)}
	}
	return json.Marshal(struct {
		Method string `json:"method"`
		Params []any  `json:"params"`
		ID     int    `json:"id"`
	}{"monitor", []any{cfg.Database, monitorID, reqs}, 1})
}

// runOnce handles a single connection. It returns how long the connection
// stayed up after its initial sync completed (0 if it never synced, however
// long the dump ran) and the error that ended it. Measuring from sync means a
// huge dump that keeps failing after >1m never resets backoff to 1s.
func runOnce(ctx context.Context, cfg Config, log *slog.Logger) (time.Duration, error) {
	var d net.Dialer
	nc, err := d.DialContext(ctx, "unix", cfg.Socket)
	if err != nil {
		return 0, err
	}
	began := time.Now()
	var syncedAt time.Time
	upSinceSync := func() time.Duration {
		if syncedAt.IsZero() {
			return 0
		}
		return time.Since(syncedAt)
	}
	defer nc.Close()
	stop := context.AfterFunc(ctx, func() { nc.Close() })
	defer stop()

	c := &conn{c: nc}
	req, err := monitorRequest(cfg)
	if err != nil {
		return 0, err
	}
	if err := c.write(req); err != nil {
		return 0, fmt.Errorf("send monitor: %w", err)
	}
	s := newSession(json.NewDecoder(bufio.NewReaderSize(nc, readBufSize)), cfg, log, c.echoReply)
	for {
		kind, err := s.readMessage()
		if err != nil {
			return upSinceSync(), err
		}
		if kind == msgReply && syncedAt.IsZero() {
			syncedAt = time.Now()
			took := syncedAt.Sub(began)
			log.Info("ovsdb initial sync complete", "duration", took)
			setState(cfg, true, took)
		}
	}
}
