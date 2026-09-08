package common

// CordonEntry is one operator cordon on an (upstream, method) cell.
type CordonEntry struct {
	Reason       string `json:"reason"`
	CordonedAtMs int64  `json:"cordonedAtMs"`
}

// ProjectCordons is upstreamId → method → entry. Values are treated as
// immutable once published: With/Without return new maps so a reader holding
// an older snapshot never observes a mutation.
type ProjectCordons map[string]map[string]CordonEntry

// CordonSnapshot is one project's operator cordon set as persisted in shared
// state. Version increases on every write so a replica can order a snapshot
// it fetched against one it already holds. Version 0 is a record that was
// never written (or was removed out of band) and is accepted as a reset.
type CordonSnapshot struct {
	Version int64          `json:"version"`
	Cordons ProjectCordons `json:"cordons"`
}

// Lookup resolves the effective cordon for (upstreamId, method): a wildcard
// ("*") cordon shadows any method-scoped one.
func (c ProjectCordons) Lookup(upstreamId, method string) (CordonEntry, bool) {
	methods, ok := c[upstreamId]
	if !ok {
		return CordonEntry{}, false
	}
	if e, ok := methods["*"]; ok {
		return e, true
	}
	e, ok := methods[method]
	return e, ok
}

// With returns a copy with entry set on (upstreamId, method). An existing
// entry keeps its CordonedAtMs so a reason edit never resets duration
// accounting; a new entry with CordonedAtMs == 0 is stamped with nowMs.
func (c ProjectCordons) With(upstreamId, method string, entry CordonEntry, nowMs int64) ProjectCordons {
	if prev, ok := c[upstreamId][method]; ok {
		entry.CordonedAtMs = prev.CordonedAtMs
	} else if entry.CordonedAtMs == 0 {
		entry.CordonedAtMs = nowMs
	}
	out := c.clone(upstreamId)
	out[upstreamId][method] = entry
	return out
}

// Without returns a copy with (upstreamId, method) removed; empty upstream
// maps are dropped.
func (c ProjectCordons) Without(upstreamId, method string) ProjectCordons {
	if _, ok := c[upstreamId][method]; !ok {
		return c
	}
	out := c.clone(upstreamId)
	delete(out[upstreamId], method)
	if len(out[upstreamId]) == 0 {
		delete(out, upstreamId)
	}
	return out
}

// clone copies the top level and the one inner map about to be mutated;
// untouched inner maps are shared because they are never written in place.
func (c ProjectCordons) clone(upstreamId string) ProjectCordons {
	out := make(ProjectCordons, len(c)+1)
	for id, methods := range c {
		out[id] = methods
	}
	inner := make(map[string]CordonEntry, len(c[upstreamId])+1)
	for m, e := range c[upstreamId] {
		inner[m] = e
	}
	out[upstreamId] = inner
	return out
}
