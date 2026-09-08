package cache

import (
	"sort"
	"sync"

	"github.com/cespare/xxhash/v2"
)

type scanKey struct {
	sequence uint64
	itemID   string
}

type valueShard struct {
	mu           sync.RWMutex
	values       map[string]ValueState
	order        []scanKey
	nextSequence uint64
	tombstones   int
}

func newValueShard(capacity int) valueShard {
	return valueShard{
		values: make(map[string]ValueState, capacity),
		order:  make([]scanKey, 0, capacity),
	}
}

// SeriesFingerprint is the stable key shared by cache sharding and publish
// slots. The current model maps one item to one Prometheus series.
func SeriesFingerprint(itemID string) uint64 {
	return xxhash.Sum64String(itemID)
}

func (s *valueShard) insert(state ValueState) {
	if current, exists := s.values[state.ItemID]; exists {
		state.scanSequence = current.scanSequence
	} else {
		s.nextSequence++
		state.scanSequence = s.nextSequence
		s.order = append(s.order, scanKey{sequence: s.nextSequence, itemID: state.ItemID})
	}
	s.values[state.ItemID] = state
}

func (s *valueShard) delete(itemID string) bool {
	if _, exists := s.values[itemID]; !exists {
		return false
	}
	delete(s.values, itemID)
	s.tombstones++
	return true
}

// compactOrder is amortized per shard and preserves sequence numbers, so an
// outstanding PublishCursor remains valid after deleted keys are removed.
func (s *valueShard) compactOrder() {
	if s.tombstones < 1024 || s.tombstones*2 < len(s.order) {
		return
	}
	live := s.order[:0]
	for _, key := range s.order {
		if state, exists := s.values[key.itemID]; exists && state.scanSequence == key.sequence {
			live = append(live, key)
		}
	}
	s.order = live
	s.tombstones = 0
}

func (s *valueShard) firstSequenceAfter(sequence uint64) int {
	return sort.Search(len(s.order), func(i int) bool {
		return s.order[i].sequence > sequence
	})
}
