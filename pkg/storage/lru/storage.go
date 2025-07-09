package lru

import (
	"context"
	"github.com/Borislavv/advanced-cache/pkg/storage/lfu"
	"math/rand/v2"
	"runtime"
	"strconv"
	"time"

	"github.com/Borislavv/advanced-cache/pkg/config"
	"github.com/Borislavv/advanced-cache/pkg/model"
	"github.com/Borislavv/advanced-cache/pkg/repository"
	sharded "github.com/Borislavv/advanced-cache/pkg/storage/map"
	"github.com/Borislavv/advanced-cache/pkg/utils"
	"github.com/rs/zerolog/log"
)

// Storage is a Weight-aware, sharded Storage cache with background eviction and refreshItem support.
type Storage struct {
	ctx             context.Context               // Main context for lifecycle control
	cfg             *config.Cache                 // CacheBox configuration
	shardedMap      *sharded.Map[*model.Response] // Sharded storage for cache entries
	tinyLFU         *lfu.TinyLFU                  // Helps hold more frequency used items in cache while eviction
	backend         repository.Backender          // Remote backend server.
	balancer        Balancer                      // Helps pick shards to evict from
	mem             int64                         // Current Weight usage (bytes)
	memoryThreshold int64                         // Threshold for triggering eviction (bytes)
}

// NewStorage constructs a new Storage cache instance and launches eviction and refreshItem routines.
func NewStorage(
	ctx context.Context,
	cfg *config.Cache,
	balancer Balancer,
	backend repository.Backender,
	tinyLFU *lfu.TinyLFU,
	shardedMap *sharded.Map[*model.Response],
) *Storage {
	return (&Storage{
		ctx:             ctx,
		cfg:             cfg,
		shardedMap:      shardedMap,
		balancer:        balancer,
		backend:         backend,
		tinyLFU:         tinyLFU,
		memoryThreshold: int64(float64(cfg.Cache.Storage.Size) * cfg.Cache.Eviction.Threshold),
	}).init()
}

func (s *Storage) init() *Storage {
	// Register all existing shards with the balancer.
	s.shardedMap.WalkShards(func(shardKey uint64, shard *sharded.Shard[*model.Response]) {
		s.balancer.Register(shard)
	})

	return s
}

func (s *Storage) Run() {
	s.runLogger()
}

// Get retrieves a response by request and bumps its Storage position.
// Returns: (response, releaser, found).
func (s *Storage) Get(key, shard uint64) (*model.Response, bool) {
	resp, found := s.shardedMap.Get(key, shard)
	if found {
		s.touch(resp)
		return resp, true
	}
	return nil, false
}

func (s *Storage) GetRandom() (resp *model.Response, isFound bool) {
	s.shardedMap.
		Shard(sharded.MapShardKey(uint64(rand.IntN(int(sharded.ActiveShards))))).
		Walk(s.ctx, func(u uint64, response *model.Response) bool {
			resp = response
			isFound = true
			return false
		}, false)

	return resp, isFound
}

// Set inserts or updates a response in the cache, updating Weight usage and Storage position.
func (s *Storage) Set(new *model.Response) {
	key := new.Request().MapKey()
	shardKey := new.Request().ShardKey()

	// Track access frequency
	s.tinyLFU.Increment(key)

	existing, found := s.shardedMap.Get(key, shardKey)
	if found {
		s.update(existing)
		return
	}

	// Admission control: if memory is over threshold, evaluate before inserting
	if s.ShouldEvict() {
		victim, ok := s.balancer.FindVictim(shardKey)
		if !ok {
			return
		}
		if victim != nil && !s.tinyLFU.Admit(new, victim) {
			// New item is less frequent than victim, skip insertion
			return
		}
	}

	// Proceed with insert
	s.set(new)
}

// touch bumps the Storage position of an existing entry (MoveToFront) and increases its refCount.
func (s *Storage) touch(existing *model.Response) {
	s.balancer.Update(existing)
}

// update refreshes Weight accounting and Storage position for an updated entry.
func (s *Storage) update(existing *model.Response) {
	s.balancer.Update(existing)
}

// set inserts a new response, updates Weight usage and registers in balancer.
func (s *Storage) set(new *model.Response) {
	s.shardedMap.Set(new)
	s.balancer.Set(new)
}

// runLogger emits detailed stats about evictions, Weight, and GC activity every 5 seconds if debugging is enabled.
func (s *Storage) runLogger() {
	go func() {
		var ticker = utils.NewTicker(s.ctx, 5*time.Second)

		for {
			select {
			case <-s.ctx.Done():
				return
			case <-ticker:
				var m runtime.MemStats
				runtime.ReadMemStats(&m)

				var (
					realMem    = s.shardedMap.Mem()
					mem        = utils.FmtMem(realMem)
					length     = strconv.Itoa(int(s.shardedMap.Len()))
					gc         = strconv.Itoa(int(m.NumGC))
					limit      = utils.FmtMem(int64(s.cfg.Cache.Storage.Size))
					goroutines = strconv.Itoa(runtime.NumGoroutine())
					alloc      = utils.FmtMem(int64(m.Alloc))
				)

				logEvent := log.Info()

				if s.cfg.IsProd() {
					logEvent.
						Str("target", "storage").
						Str("mem", strconv.Itoa(int(realMem))).
						Str("memStr", mem).
						Str("len", length).
						Str("gc", gc).
						Str("memLimit", strconv.Itoa(int(s.cfg.Cache.Storage.Size))).
						Str("memLimitStr", limit).
						Str("goroutines", goroutines).
						Str("alloc", strconv.Itoa(int(m.Alloc))).
						Str("allocStr", alloc)
				}

				logEvent.Msgf("[storage][5s] usage: %s, len: %s, limit: %s, alloc: %s, goroutines: %s, gc: %s",
					mem, length, limit, alloc, goroutines, gc)
			}
		}
	}()
}

func (s *Storage) Remove(resp *model.Response) (freedBytes int64, isHit bool) {
	s.balancer.Remove(resp.ShardKey(), resp.LruListElement())
	return s.shardedMap.Remove(resp.MapKey())
}

func (s *Storage) Mem() int64 {
	return s.shardedMap.Mem()
}
func (s *Storage) RealMem() int64 {
	return s.shardedMap.RealMem()
}

func (s *Storage) Stat() (bytes int64, length int64) {
	return s.shardedMap.Mem(), s.shardedMap.Len()
}

// ShouldEvict [HOT PATH METHOD] (max stale value = 25ms) checks if current Weight usage has reached or exceeded the threshold.
func (s *Storage) ShouldEvict() bool {
	return s.Mem() >= s.memoryThreshold
}
