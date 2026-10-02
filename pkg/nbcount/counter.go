package nbcount

import "sync"

// Key is the label tuple rows are bucketed by.
type Key struct{ OwnerType, Network string }

type tableState struct {
	rows   map[[16]byte]uint32 // uuid -> key index
	keys   []Key               // interned keys
	index  map[Key]uint32      // key -> key index
	counts []int               // parallel to keys
}

func newTable() *tableState {
	return &tableState{rows: map[[16]byte]uint32{}, index: map[Key]uint32{}}
}

func (t *tableState) intern(k Key) uint32 {
	if i, ok := t.index[k]; ok {
		return i
	}
	i := uint32(len(t.keys))
	t.keys = append(t.keys, k)
	t.counts = append(t.counts, 0)
	t.index[k] = i
	return i
}

// Counter tracks row counts per table and Key. Safe for concurrent use.
type Counter struct {
	mu      sync.Mutex
	tables  map[string]*tableState
	updates map[[2]string]uint64
}

// NewCounter returns an empty Counter.
func NewCounter() *Counter {
	return &Counter{tables: map[string]*tableState{}, updates: map[[2]string]uint64{}}
}

// Upsert records row uuid in table under k and returns "insert" for a new row
// or "modify" for a known one (moving it between buckets if k changed).
func (c *Counter) Upsert(table string, uuid [16]byte, k Key) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.tables[table]
	if t == nil {
		t = newTable()
		c.tables[table] = t
	}
	ki := t.intern(k)
	if old, ok := t.rows[uuid]; ok {
		if old != ki {
			t.counts[old]--
			t.counts[ki]++
			t.rows[uuid] = ki
		}
		return "modify"
	}
	t.rows[uuid] = ki
	t.counts[ki]++
	return "insert"
}

// Delete removes a row; it reports false (and changes nothing) if unknown.
func (c *Counter) Delete(table string, uuid [16]byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.tables[table]
	if t == nil {
		return false
	}
	ki, ok := t.rows[uuid]
	if !ok {
		return false
	}
	delete(t.rows, uuid)
	t.counts[ki]--
	return true
}

// RecordUpdate increments the {table, op} update counter.
func (c *Counter) RecordUpdate(table, op string) {
	c.mu.Lock()
	c.updates[[2]string{table, op}]++
	c.mu.Unlock()
}

// Reset clears all rows but keeps update counters.
func (c *Counter) Reset() {
	c.mu.Lock()
	c.tables = map[string]*tableState{}
	c.mu.Unlock()
}

// Snapshot returns table -> key -> count, omitting zero counts.
func (c *Counter) Snapshot() map[string]map[Key]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]map[Key]int, len(c.tables))
	for name, t := range c.tables {
		m := map[Key]int{}
		for i, n := range t.counts {
			if n > 0 {
				m[t.keys[i]] = n
			}
		}
		out[name] = m
	}
	return out
}

// Updates returns a copy of the {table, op} -> total update counters.
func (c *Counter) Updates() map[[2]string]uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[[2]string]uint64, len(c.updates))
	for k, v := range c.updates {
		out[k] = v
	}
	return out
}
