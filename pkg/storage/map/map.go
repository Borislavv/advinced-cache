package sharded

import (
	"context"
	"github.com/Borislavv/traefik-http-cache-plugin/pkg/resource"
	"sync"
	"sync/atomic"
)

const NumOfShards uint64 = 2048 // Total number of shards (power of 2 for fast hashing)

// Map is a sharded concurrent map for high-performance caches.
type Map[V resource.Resource] struct {
	len    int64
	mem    int64
	shards [NumOfShards]*Shard[V]
}

// NewMap creates a new sharded map with preallocated shards and a default per-shard map capacity.
func NewMap[V resource.Resource](defaultLen int) *Map[V] {
	m := &Map[V]{}
	for id := uint64(0); id < NumOfShards; id++ {
		m.shards[id] = NewShard[V](id, defaultLen)
	}
	return m
}

// MapShardKey calculates the shard index for a given key.
func MapShardKey(key uint64) uint64 {
	return key % NumOfShards
}

// Set inserts or updates a value in the correct shard. Returns a releaser for ref counting.
func (smap *Map[V]) Set(value V) {
	takenMem := smap.shards[value.ShardKey()].Set(value.MapKey(), value)
	atomic.AddInt64(&smap.len, 1)
	atomic.AddInt64(&smap.mem, takenMem)
}

// Get fetches a value and its releaser from the correct shard.
// found==false means the value is absent.
func (smap *Map[V]) Get(key uint64, shardKey uint64) (V, bool) {
	return smap.shards[shardKey].Get(key)
}

func (smap *Map[V]) Update(old, new V) {
	atomic.AddInt64(&smap.mem, new.Weight()-old.Weight())
}

// Remove deletes a value by key, returning how much memory was freed and a pointer to its LRU/list element.
func (smap *Map[V]) Remove(key uint64) (freed int64, isHit bool) {
	freed, isHit = smap.Shard(key).Remove(key)
	if isHit {
		atomic.AddInt64(&smap.len, -1)
		atomic.AddInt64(&smap.mem, -freed)
	}
	return freed, isHit
}

// Walk applies fn to all key/value pairs in the shard, optionally locking for writing.
func (shard *Shard[V]) Walk(ctx context.Context, fn func(key uint64, value V) bool, lockRead bool) {
	if lockRead {
		shard.Lock()
		defer shard.Unlock()
	} else {
		shard.RLock()
		defer shard.RUnlock()
	}
	for k, v := range shard.items {
		select {
		case <-ctx.Done():
			return
		default:
			if ok := fn(k, v); !ok {
				return
			}
		}
	}
}

// Shard returns the shard that stores the given key.
func (smap *Map[V]) Shard(key uint64) *Shard[V] {
	return smap.shards[MapShardKey(key)]
}

// WalkShards launches fn concurrently for each shard (key, *Shard[V]).
// The callback runs in a separate goroutine for each shard; fn should be goroutine-safe.
func (smap *Map[V]) WalkShards(fn func(key uint64, shard *Shard[V])) {
	var wg sync.WaitGroup
	wg.Add(int(NumOfShards))
	defer wg.Wait()
	for k, s := range smap.shards {
		go func(key uint64, shard *Shard[V]) {
			defer wg.Done()
			fn(key, shard)
		}(uint64(k), s)
	}
}

// Len returns the total number of elements in all shards (O(NumOfShards)).
func (smap *Map[V]) Len() int64 {
	return atomic.LoadInt64(&smap.len)
}

func (smap *Map[V]) Mem() int64 {
	return atomic.LoadInt64(&smap.mem)
}
