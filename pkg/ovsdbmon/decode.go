package ovsdbmon

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"unicode/utf8"

	"github.com/cragr/ovnk-observ/pkg/nbcount"
)

var errMonitorCanceled = errors.New("ovsdb monitor canceled by server")

// msgKind classifies one decoded JSON-RPC message.
type msgKind int

const (
	msgOther  msgKind = iota // ignored notification or unknown message
	msgReply                 // the (successful) monitor reply
	msgUpdate                // an "update" notification, already applied
	msgEcho                  // an "echo" request, already answered
)

// rowUpdate is one <row-update> object. A nil map means the member was absent.
type rowUpdate struct {
	Old map[string]json.RawMessage `json:"old"`
	New map[string]json.RawMessage `json:"new"`
}

// session decodes one connection's message stream and applies rows to the
// counter one at a time. The monitor reply and update notifications are walked
// with Token() so that only a single row-update is ever materialized.
type session struct {
	dec     *json.Decoder
	tables  map[string]TableSpec
	counter *nbcount.Counter
	hook    RowHook
	log     *slog.Logger
	echo    func(id, params json.RawMessage) error

	badUUIDLogged bool
}

func newSession(dec *json.Decoder, cfg Config, log *slog.Logger, echo func(id, params json.RawMessage) error) *session {
	t := make(map[string]TableSpec, len(cfg.Tables))
	for _, ts := range cfg.Tables {
		t[ts.Name] = ts
	}
	return &session{dec: dec, tables: t, counter: cfg.Counter, hook: cfg.Hook, log: log, echo: echo}
}

// readMessage decodes and handles exactly one top-level JSON-RPC message.
// It returns an error for malformed input, I/O failure, or an error reply.
func (s *session) readMessage() (msgKind, error) {
	dec := s.dec
	if err := expectDelim(dec, '{'); err != nil {
		return msgOther, err
	}
	var (
		method          string
		id, params, rpc json.RawMessage
		hadResult       bool
		paramsStreamed  bool
	)
	for dec.More() {
		key, err := stringToken(dec)
		if err != nil {
			return msgOther, err
		}
		switch key {
		case "method":
			if err := dec.Decode(&method); err != nil {
				return msgOther, fmt.Errorf("decode method: %w", err)
			}
		case "id":
			if err := dec.Decode(&id); err != nil {
				return msgOther, err
			}
		case "error":
			if err := dec.Decode(&rpc); err != nil {
				return msgOther, err
			}
		case "result":
			hadResult = true
			if err := s.streamTableUpdates(dec, true); err != nil {
				return msgOther, fmt.Errorf("monitor reply: %w", err)
			}
		case "params":
			if method == "update" {
				if err := s.streamUpdateParams(dec); err != nil {
					return msgOther, fmt.Errorf("update: %w", err)
				}
				paramsStreamed = true
			} else if err := dec.Decode(&params); err != nil {
				// method unknown yet (or not update): small, buffer it.
				return msgOther, err
			}
		default:
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return msgOther, err
			}
		}
	}
	if err := expectDelim(dec, '}'); err != nil {
		return msgOther, err
	}

	switch method {
	case "update":
		if !paramsStreamed && params != nil {
			// params preceded method in the object; replay the buffered copy.
			if err := s.streamUpdateParams(json.NewDecoder(bytes.NewReader(params))); err != nil {
				return msgOther, fmt.Errorf("update: %w", err)
			}
		}
		return msgUpdate, nil
	case "echo":
		if len(id) == 0 {
			id = json.RawMessage("null")
		}
		if len(params) == 0 {
			params = json.RawMessage("[]")
		}
		return msgEcho, s.echo(id, params)
	case "monitor_canceled":
		// The server dropped our monitor but kept the socket open; no more
		// updates will arrive, so end the session and let Run reset and
		// reconnect rather than serve frozen counts as connected.
		return msgOther, errMonitorCanceled
	case "":
		if isNull(rpc) {
			if hadResult {
				return msgReply, nil
			}
			return msgOther, nil
		}
		return msgOther, fmt.Errorf("ovsdb error reply (id %s): %s", id, rpc)
	default:
		s.log.Debug("ignoring ovsdb notification", "method", method)
		return msgOther, nil
	}
}

// streamUpdateParams walks ["<monitor-id>", <table-updates>].
func (s *session) streamUpdateParams(dec *json.Decoder) error {
	if err := expectDelim(dec, '['); err != nil {
		return err
	}
	var monID json.RawMessage
	if err := dec.Decode(&monID); err != nil {
		return err
	}
	if err := s.streamTableUpdates(dec, false); err != nil {
		return err
	}
	for dec.More() { // tolerate trailing elements
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return err
		}
	}
	return expectDelim(dec, ']')
}

// streamTableUpdates walks {<table>: {<uuid>: <row-update>}} (or null),
// decoding and applying one row-update at a time.
func (s *session) streamTableUpdates(dec *json.Decoder, initial bool) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok == nil {
		return nil
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("table-updates: unexpected token %v", tok)
	}
	for dec.More() {
		table, err := stringToken(dec)
		if err != nil {
			return err
		}
		spec, known := s.tables[table]
		if err := expectDelim(dec, '{'); err != nil {
			return err
		}
		for dec.More() {
			uuid, err := stringToken(dec)
			if err != nil {
				return err
			}
			var ru rowUpdate
			if err := dec.Decode(&ru); err != nil {
				return fmt.Errorf("table %s row %s: %w", table, uuid, err)
			}
			if known {
				s.apply(spec, uuid, &ru, initial)
			}
		}
		if err := expectDelim(dec, '}'); err != nil {
			return err
		}
	}
	return expectDelim(dec, '}')
}

func (s *session) apply(spec TableSpec, uuidStr string, ru *rowUpdate, initial bool) {
	u, err := nbcount.ParseUUID(uuidStr)
	if err != nil {
		if !s.badUUIDLogged {
			s.badUUIDLogged = true
			s.log.Warn("skipping row with unparseable uuid (logged once per connection)", "table", spec.Name, "err", err)
		}
		return
	}
	switch {
	case ru.New != nil:
		s.counter.Upsert(spec.Name, u, rowKey(spec, ru.New))
		if s.hook != nil {
			s.hook.Upsert(spec.Name, u, ru.New)
		}
		switch {
		case initial:
		case ru.Old == nil:
			s.counter.RecordUpdate(spec.Name, "insert")
		case changesBase(spec, ru.Old):
			s.counter.RecordUpdate(spec.Name, "modify")
		}
	case ru.Old != nil && !initial:
		s.counter.Delete(spec.Name, u)
		if s.hook != nil {
			s.hook.Delete(spec.Name, u)
		}
		s.counter.RecordUpdate(spec.Name, "delete")
	}
}

// changesBase reports whether a modify's "old" (which in monitor v1 lists only
// the changed columns) touches a column outside spec.Extra. Changes confined to
// Extra columns are not counted as NB updates, so monitoring extra columns does
// not change the base updates metric.
func changesBase(spec TableSpec, old map[string]json.RawMessage) bool {
	for col := range old {
		if !slices.Contains(spec.Extra, col) {
			return true
		}
	}
	return false
}

var constKey = nbcount.Key{OwnerType: "none", Network: "default"}

func rowKey(spec TableSpec, row map[string]json.RawMessage) nbcount.Key {
	if !spec.KeyFromRow {
		return constKey
	}
	owner, network := nbcount.ParseExternalIDs(parseMap(row[spec.Column]))
	return nbcount.Key{OwnerType: owner, Network: network}
}

// parseMap decodes an OVSDB <map> of strings (["map",[["k","v"],...]]).
// Anything else yields an empty map; non-string pairs are skipped.
// It runs once per NB row, so the common shape is parsed by hand; any input
// the fast path does not fully understand goes through encoding/json.
func parseMap(raw json.RawMessage) map[string]string {
	if m, ok := parseMapFast(raw); ok {
		return m
	}
	return parseMapSlow(raw)
}

func parseMapSlow(raw json.RawMessage) map[string]string {
	var v []json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil || len(v) != 2 {
		return nil
	}
	var tag string
	if json.Unmarshal(v[0], &tag) != nil || tag != "map" {
		return nil
	}
	var pairs [][]json.RawMessage
	if json.Unmarshal(v[1], &pairs) != nil {
		return nil
	}
	m := make(map[string]string, len(pairs))
	for _, p := range pairs {
		if len(p) != 2 {
			continue
		}
		var k, val string
		if json.Unmarshal(p[0], &k) != nil || json.Unmarshal(p[1], &val) != nil {
			continue
		}
		m[k] = val
	}
	return m
}

// parseMapFast handles a well-formed map whose keys and values are all
// strings. ok=false means "not handled here", not "invalid".
func parseMapFast(raw []byte) (m map[string]string, ok bool) {
	sc := mapScanner{b: raw}
	if !sc.lit('[') {
		return nil, false
	}
	if tag, ok := sc.str(); !ok || tag != "map" {
		return nil, false
	}
	if !sc.lit(',') || !sc.lit('[') {
		return nil, false
	}
	m = make(map[string]string, 4)
	if !sc.lit(']') {
		for {
			if !sc.lit('[') {
				return nil, false
			}
			k, ok1 := sc.str()
			if !ok1 || !sc.lit(',') {
				return nil, false
			}
			v, ok2 := sc.str()
			if !ok2 || !sc.lit(']') {
				return nil, false
			}
			m[k] = v
			if sc.lit(',') {
				continue
			}
			if sc.lit(']') {
				break
			}
			return nil, false
		}
	}
	if !sc.lit(']') {
		return nil, false
	}
	sc.ws()
	return m, sc.i == len(sc.b)
}

type mapScanner struct {
	b []byte
	i int
}

func (s *mapScanner) ws() {
	for s.i < len(s.b) {
		switch s.b[s.i] {
		case ' ', '\t', '\n', '\r':
			s.i++
		default:
			return
		}
	}
}

func (s *mapScanner) lit(c byte) bool {
	s.ws()
	if s.i < len(s.b) && s.b[s.i] == c {
		s.i++
		return true
	}
	return false
}

// str reads a JSON string. Plain ASCII/UTF-8 strings are converted directly;
// strings with escapes or invalid UTF-8 are delegated to encoding/json.
func (s *mapScanner) str() (string, bool) {
	s.ws()
	if s.i >= len(s.b) || s.b[s.i] != '"' {
		return "", false
	}
	start := s.i
	s.i++
	simple := true
	for s.i < len(s.b) {
		c := s.b[s.i]
		switch {
		case c == '\\':
			simple = false
			s.i += 2
			continue
		case c == '"':
			s.i++
			lit := s.b[start:s.i]
			if simple && utf8.Valid(lit) {
				return string(lit[1 : len(lit)-1]), true
			}
			var out string
			if json.Unmarshal(lit, &out) != nil {
				return "", false
			}
			return out, true
		case c < 0x20:
			return "", false
		}
		s.i++
	}
	return "", false
}

func expectDelim(dec *json.Decoder, want json.Delim) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != want {
		return fmt.Errorf("expected %q, got %v", want, tok)
	}
	return nil
}

func stringToken(dec *json.Decoder) (string, error) {
	tok, err := dec.Token()
	if err != nil {
		return "", err
	}
	s, ok := tok.(string)
	if !ok {
		return "", errors.New("expected string token")
	}
	return s, nil
}

func isNull(raw json.RawMessage) bool {
	return len(raw) == 0 || string(bytes.TrimSpace(raw)) == "null"
}
