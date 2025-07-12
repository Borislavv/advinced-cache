package lru

import (
	"context"
	list2 "github.com/Borislavv/advanced-cache/pkg/list"
	"github.com/Borislavv/advanced-cache/pkg/model"
	sharded "github.com/Borislavv/advanced-cache/pkg/storage/map"
	"math/rand/v2"
	"unsafe"
)

// ShardNode represents a single Shard's Storage and accounting info.
// Each Shard has its own Storage list and a pointer to its element in the balancer's memList.
type ShardNode struct {
	lruList     *list2.List[*model.Entry]    // Per-Shard Storage list; less used responses at the back
	memListElem *list2.Element[*ShardNode]   // Pointer to this node's position in Balance.memList
	Shard       *sharded.Shard[*model.Entry] // Reference to the actual Shard (map + sync)
}

func (s *ShardNode) RandItem(ctx context.Context) *model.Entry {
	var entry *model.Entry
	s.Shard.Walk(ctx, func(u uint64, entryIter *model.Entry) bool {
		entry = entryIter
		return false
	}, false)
	return entry
}

// Weight returns an approximate Weight usage of this ShardNode structure.
func (s *ShardNode) Weight() int64 {
	return s.Shard.Weight()
}

func (s *ShardNode) LruList() *list2.List[*model.Entry] {
	return s.lruList
}

type Balancer interface {
	Rebalance()
	Mem() int64
	Shards() [sharded.NumOfShards]*ShardNode
	RandNode() *ShardNode
	Register(shard *sharded.Shard[*model.Entry])
	Set(entry *model.Entry)
	Update(existing *model.Entry)
	Move(shardKey uint64, el *list2.Element[*model.Entry])
	Remove(shardKey uint64, el *list2.Element[*model.Entry])
	MostLoadedSampled(offset int) (*ShardNode, bool)
	FindVictim(shardKey uint64) (*model.Entry, bool)
}

// Balance maintains per-Shard Storage lists and provides efficient selection of loaded shards for eviction.
// - memList orders shardNodes by usage (most loaded in front).
// - shards is a flat array for O(1) access by Shard index.
// - shardedMap is the underlying data storage (map of all entries).
type Balance struct {
	ctx        context.Context
	shards     [sharded.NumOfShards]*ShardNode // Shard index → *ShardNode
	memList    *list2.List[*ShardNode]         // Doubly-linked list of shards, ordered by Memory usage (most loaded at front)
	shardedMap *sharded.Map[*model.Entry]      // Actual underlying storage of entries
}

var ptrBytesSize uint64 = 8

func (b *Balance) Mem() int64 {
	mem := int64(uint64(unsafe.Sizeof(*b)) + (sharded.NumOfShards * ptrBytesSize) + (uint64(b.memList.Len()) * ptrBytesSize))
	if shard := b.shards[0]; shard != nil {
		mem += int64(uint64(unsafe.Sizeof(*shard)) * sharded.NumOfShards)
	}
	return mem
}

// NewBalancer creates a new Balance instance and initializes memList.
func NewBalancer(ctx context.Context, shardedMap *sharded.Map[*model.Entry]) *Balance {
	return &Balance{
		ctx:        ctx,
		memList:    list2.New[*ShardNode](), // Sorted mode for easier rebalancing
		shardedMap: shardedMap,
	}
}

func (b *Balance) Rebalance() {
	// sort shardNodes by weight (freedMem)
	b.memList.Sort(list2.DESC)
}

func (b *Balance) Shards() [sharded.NumOfShards]*ShardNode {
	return b.shards
}

// RandNode returns a random ShardNode for sampling (e.g., for background refreshers).
func (b *Balance) RandNode() *ShardNode {
	if n := b.shards[rand.Uint64N(sharded.NumOfShards)]; n.Shard.ID() == 0 {
		return b.shards[rand.Uint64N(sharded.ActiveShards)]
	} else {
		return n
	}
}

// Register inserts a new ShardNode for a given Shard, creates its Storage, and adds it to memList and shards array.
func (b *Balance) Register(shard *sharded.Shard[*model.Entry]) {
	n := &ShardNode{
		Shard:   shard,
		lruList: list2.New[*model.Entry](),
	}
	n.memListElem = b.memList.PushBack(n)
	b.shards[shard.ID()] = n
}

// Set inserts a response into the appropriate Shard's Storage list and updates counters.
// Returns the affected ShardNode for further operations.
func (b *Balance) Set(entry *model.Entry) {
	entry.SetLruListElement(b.shards[entry.ShardKey()].lruList.PushFront(entry))
}

func (b *Balance) Update(existing *model.Entry) {
	b.shards[existing.ShardKey()].lruList.MoveToFront(existing.LruListElement())
}

// Move moves an element to the front of the per-Shard Storage list.
// Used for touch/Set operations to mark entries as most recently used.
func (b *Balance) Move(shardKey uint64, el *list2.Element[*model.Entry]) {
	b.shards[shardKey].lruList.MoveToFront(el)
}

func (b *Balance) Remove(shardKey uint64, el *list2.Element[*model.Entry]) {
	b.shards[shardKey].lruList.Remove(el)
}

// MostLoadedSampled returns the first non-empty Shard node from the front of memList,
// optionally skipping a number of nodes by offset (for concurrent eviction fairness).
func (b *Balance) MostLoadedSampled(offset int) (*ShardNode, bool) {
	el, ok := b.memList.Next(offset)
	if !ok {
		return nil, false
	}
	return el.Value(), ok
}

func (b *Balance) FindVictim(shardKey uint64) (*model.Entry, bool) {
	shardKeyInt64 := int64(shardKey)
	if el := b.shards[shardKeyInt64].lruList.Back(); el != nil {
		return el.Value(), true
	}
	if int64(len(b.shards)) > shardKeyInt64+1 {
		if el := b.shards[shardKeyInt64+1].lruList.Back(); el != nil {
			return el.Value(), true
		}
	}
	if shardKeyInt64-1 > 0 {
		if el := b.shards[shardKeyInt64-1].lruList.Back(); el != nil {
			return el.Value(), true
		}
	}
	return nil, false
}
