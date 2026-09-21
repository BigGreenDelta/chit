package store

import (
	"encoding/json"

	"ledger/internal/dag"
	"ledger/internal/model"
)

// EventsOfNodes reads just these nodes' events, keyed by 10-char event id  -
// the lookup table the cursor contract needs for one RANGE, without the
// whole-chain read Events() pays for. One batch, the same sentinel rules
// every other read applies: a commit whose event.json is missing or
// unparseable simply has no entry, and the caller contracts it out.
//
// Deliberately NOT a fold: it returns no order at all. The batch order is
// dag.Sort's job, over the range's own contracted sub-DAG, and giving this
// a slice return would invite a caller to trust the wrong order.
func (s Store) EventsOfNodes(nodes []dag.Node) (map[string]model.Event, error) {
	byID := make(map[string]model.Event, len(nodes))
	if len(nodes) == 0 {
		return byID, nil
	}
	reqs := make([]string, len(nodes))
	for i, n := range nodes {
		reqs[i] = n.SHA + ":event.json"
	}
	contents, present := s.catBatch(reqs)
	for i, n := range nodes {
		if !present[i] {
			continue
		}
		var ev model.Event
		if json.Unmarshal([]byte(contents[i]), &ev) != nil {
			continue
		}
		if ev.Type == "sync" {
			// A sync sentinel is contracted out of every read, and the
			// caller detects that by ABSENCE from this table. Whole-chain
			// Events() produced the same absence by dropping sentinels from
			// the fold order; reproducing it here is what keeps a sync merge
			// from being delivered as an event.
			continue
		}
		ev.ID = n.SHA[:10]
		byID[ev.ID] = ev
	}
	return byID, nil
}
