// Package pgdrift counts NB Port_Groups that have ports but no matching SB
// Port_Group, fed from the streaming OVSDB monitors of both databases.
package pgdrift

import (
	"encoding/json"
	"hash/fnv"
	"sync"
)

const table = "Port_Group"

type nbRow struct {
	hash     uint64
	hasPorts bool
}

// Tracker holds the NB and SB Port_Group state. Safe for concurrent use.
type Tracker struct {
	mu     sync.Mutex
	nb     map[[16]byte]nbRow
	sb     map[[16]byte]uint64
	sbRefs map[uint64]uint32 // base-name hash -> number of SB rows
	nbSide *Side
	sbSide *Side
}

// Side feeds one database's rows into a Tracker. Its methods have the shape
// of the ovsdbmon row hook.
type Side struct {
	t    *Tracker
	isSB bool
}

// NewTracker returns an empty Tracker.
func NewTracker() *Tracker {
	t := &Tracker{
		nb:     map[[16]byte]nbRow{},
		sb:     map[[16]byte]uint64{},
		sbRefs: map[uint64]uint32{},
	}
	t.nbSide = &Side{t: t}
	t.sbSide = &Side{t: t, isSB: true}
	return t
}

// NB returns the side that receives Northbound rows.
func (t *Tracker) NB() *Side { return t.nbSide }

// SB returns the side that receives Southbound rows.
func (t *Tracker) SB() *Side { return t.sbSide }

// Upsert records an inserted or modified row. Tables other than Port_Group
// are ignored.
func (s *Side) Upsert(tbl string, uuid [16]byte, row map[string]json.RawMessage) {
	if tbl != table {
		return
	}
	name := decodeName(row["name"])
	t := s.t
	t.mu.Lock()
	defer t.mu.Unlock()
	if s.isSB {
		if old, ok := t.sb[uuid]; ok {
			t.decRef(old)
		}
		h := hashName(SBBaseName(name))
		t.sb[uuid] = h
		t.sbRefs[h]++
		return
	}
	t.nb[uuid] = nbRow{hash: hashName(name), hasPorts: HasPorts(row["ports"])}
}

// Delete removes a row. Tables other than Port_Group are ignored.
func (s *Side) Delete(tbl string, uuid [16]byte) {
	if tbl != table {
		return
	}
	t := s.t
	t.mu.Lock()
	defer t.mu.Unlock()
	if s.isSB {
		if old, ok := t.sb[uuid]; ok {
			t.decRef(old)
			delete(t.sb, uuid)
		}
		return
	}
	delete(t.nb, uuid)
}

// Reset drops all rows of this side, e.g. before a monitor resync.
func (s *Side) Reset() {
	t := s.t
	t.mu.Lock()
	defer t.mu.Unlock()
	if s.isSB {
		t.sb = map[[16]byte]uint64{}
		t.sbRefs = map[uint64]uint32{}
		return
	}
	t.nb = map[[16]byte]nbRow{}
}

func (t *Tracker) decRef(h uint64) {
	if t.sbRefs[h] <= 1 {
		delete(t.sbRefs, h)
		return
	}
	t.sbRefs[h]--
}

// Missing returns how many NB Port_Groups with ports have no SB counterpart,
// and how many NB Port_Groups have ports at all.
func (t *Tracker) Missing() (missing, withPorts int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, r := range t.nb {
		if !r.hasPorts {
			continue
		}
		withPorts++
		if t.sbRefs[r.hash] == 0 {
			missing++
		}
	}
	return missing, withPorts
}

// HasPorts reports whether a raw OVSDB ports value (a uuid or a set) is
// non-empty, without unmarshalling the whole value.
func HasPorts(raw json.RawMessage) bool {
	i := skipSpace(raw, 0)
	if i >= len(raw) || raw[i] != '[' {
		return false
	}
	i = skipSpace(raw, i+1)
	if i >= len(raw) || raw[i] != '"' {
		return false
	}
	j := i + 1
	for j < len(raw) && raw[j] != '"' {
		j++
	}
	switch string(raw[i+1 : j]) {
	case "uuid":
		return true
	case "set":
		// find the inner '[' after the comma
		for j++; j < len(raw) && raw[j] != '['; j++ {
		}
		k := skipSpace(raw, j+1)
		return j < len(raw) && k < len(raw) && raw[k] != ']'
	}
	return false
}

func skipSpace(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	return i
}

// SBBaseName strips a leading "<digits>_" prefix, returning the input
// unchanged when there is none.
func SBBaseName(name string) string {
	i := 0
	for i < len(name) && name[i] >= '0' && name[i] <= '9' {
		i++
	}
	if i > 0 && i < len(name) && name[i] == '_' {
		return name[i+1:]
	}
	return name
}

func decodeName(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

func hashName(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum64()
}
