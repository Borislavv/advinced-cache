package sharded

import (
	"sync"
	"sync/atomic"
)

// Shard is a single partition of the sharded map.
// Each shard is an independent concurrent map with its own lock and refCounted pool for releasers.
type Shard[V Value] struct {
	*sync.RWMutex              // Shard-level RWMutex for concurrency
	items         map[uint64]V // Actual storage: key -> Value
	id            uint64       // Shard ID (index)
	mem           int64        // Weight usage in bytes (atomic)
	len           int64        // Length as int64 for use it as atomic
}

// NewShard creates a new shard with its own lock, value map, and releaser pool.
func NewShard[V Value](id uint64, defaultLen int) *Shard[V] {
	return &Shard[V]{
		id:      id,
		RWMutex: &sync.RWMutex{},
		items:   make(map[uint64]V, defaultLen),
	}
}

// ID returns the numeric index of this shard.
func (shard *Shard[V]) ID() uint64 {
	return shard.id
}

// Weight returns an approximate total memory usage for this shard (including overhead).
func (shard *Shard[V]) Weight() int64 {
	return atomic.LoadInt64(&shard.mem)
}

func (shard *Shard[V]) Len() int64 {
	return atomic.LoadInt64(&shard.len)
}

// Set inserts or updates a value by key, resets refCount, and updates counters.
// Returns a releaser for the inserted value.
func (shard *Shard[V]) Set(key uint64, value V) (takenMem int64) {
	shard.RLock()
	entry, isHit := shard.items[key]
	shard.RUnlock()

	if isHit {
		if !entry.IsSameFingerprint(value.Fingerprint()) {
			// hash collision, just rewrite by new data
			shard.Lock()
			shard.items[key] = value
			shard.Unlock()
			atomic.AddInt64(&shard.len, 0)
			atomic.AddInt64(&shard.mem, value.Weight()-entry.Weight())
			return
		}
	} else {
		// insert new element due to it was not found in cache
		shard.Lock()
		shard.items[key] = value
		shard.Unlock()
		atomic.AddInt64(&shard.len, 1)
		atomic.AddInt64(&shard.mem, value.Weight())
		return
	}

	// found the same, no changes has been made
	return 0
}

// Get retrieves a value and returns a releaser for it, incrementing its refCount.
// Returns (value, releaser, true) if found; otherwise (zero, nil, false).
func (shard *Shard[V]) Get(entry V) (val V, isHit bool) {
	shard.RLock()
	value, ok := shard.items[entry.MapKey()]
	shard.RUnlock()

	if ok && value.IsSameFingerprint(entry.Fingerprint()) {
		return value, ok
	}

	// not found or hash collision
	return val, false
}

// Remove removes a value from the shard, decrements counters, and may trigger full resource cleanup.
// Returns (memory_freed, pointer_to_list_element, was_found).
func (shard *Shard[V]) Remove(key uint64) (freed int64, isHit bool) {
	shard.Lock()
	defer shard.Unlock()
	v, ok := shard.items[key]
	if ok {
		delete(shard.items, key)

		weight := v.Weight()
		atomic.AddInt64(&shard.len, -1)
		atomic.AddInt64(&shard.mem, -weight)

		// return all buffers back to pools
		v.Release() // it's safe due to we are here alone, no one else could be here inside positive

		return weight, true
	}

	return 0, false
}
