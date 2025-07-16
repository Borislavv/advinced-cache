package storage

import (
	"github.com/Borislavv/advanced-cache/pkg/repository"
	"github.com/rs/zerolog"
	"runtime"
	"testing"
	"time"

	"github.com/Borislavv/advanced-cache/pkg/config"
	"github.com/Borislavv/advanced-cache/pkg/mock"
	"github.com/Borislavv/advanced-cache/pkg/model"
	"github.com/dgraph-io/ristretto"
)

var path = []byte("/api/v2/pagedata")

func init() {
	zerolog.SetGlobalLevel(zerolog.ErrorLevel)
}

type RistrettoStorage struct {
	c *ristretto.Cache
}

func NewRistrettoStorage(maxCost int64) *RistrettoStorage {
	cache, err := ristretto.NewCache(&ristretto.Config{
		NumCounters: 10_000_000,
		MaxCost:     maxCost,
		BufferItems: 256,
		Metrics:     true,
	})
	if err != nil {
		panic(err)
	}
	return &RistrettoStorage{c: cache}
}

func (r *RistrettoStorage) Set(entry *model.Entry) {
	r.c.Set(entry.MapKey(), entry, entry.Weight())
}

func (r *RistrettoStorage) Get(entry *model.Entry) (*model.Entry, bool) {
	val, ok := r.c.Get(entry.MapKey())
	if !ok {
		return nil, false
	}
	return val.(*model.Entry), true
}

var ristrettoCfg *config.Cache

func init() {
	ristrettoCfg = &config.Cache{
		Cache: config.CacheBox{
			Enabled: true,
			LifeTime: config.Lifetime{
				MaxReqDuration:             time.Millisecond * 100,
				EscapeMaxReqDurationHeader: "X-Target-Bot",
			},
			Upstream: config.Upstream{
				Url:     "https://google.com",
				Rate:    1000,
				Timeout: time.Second * 5,
			},
			Preallocate: config.Preallocation{
				PerShard: 8,
			},
			Eviction: config.Eviction{
				Policy:    "lru",
				Threshold: 0.9,
			},
			Refresh: config.Refresh{
				TTL:      time.Hour,
				ErrorTTL: time.Minute * 10,
				Beta:     0.4,
				MinStale: time.Minute * 40,
			},
			Storage: config.Storage{
				Type: "malloc",
				Size: 1024 * 1024 * 5, // 5 MB
			},
			Rules: []*config.Rule{
				{
					Path:      "/api/v2/pagedata",
					PathBytes: []byte("/api/v2/pagedata"),
					TTL:       time.Hour,
					ErrorTTL:  time.Minute * 15,
					Beta:      0.4,
					MinStale:  time.Duration(float64(time.Hour) * 0.4),
					CacheKey: config.Key{
						Query:      []string{"project[id]", "domain", "language", "choice"},
						QueryBytes: [][]byte{[]byte("project[id]"), []byte("domain"), []byte("language"), []byte("choice")},
						Headers:    []string{"Accept-Encoding", "Accept-Language"},
						HeadersMap: map[string]struct{}{
							"Accept-Encoding": {},
							"Accept-Language": {},
						},
					},
					CacheValue: config.Value{
						Headers: []string{"Content-Type", "Vary"},
						HeadersMap: map[string]struct{}{
							"Content-Type": {},
							"Vary":         {},
						},
					},
				},
			},
		},
	}
}

func reportMemAndRistretto(b *testing.B, store *RistrettoStorage) {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	b.ReportMetric(float64(mem.Alloc)/1024/1024, "allocsMB")

	stats := store.c.Metrics
	b.ReportMetric(float64(stats.CostAdded())/1024/1024, "ristrettoMB")
}

// -- BENCHMARKS

func BenchmarkRistrettoRead1000x(b *testing.B) {
	store := NewRistrettoStorage(int64(ristrettoCfg.Cache.Storage.Size))

	numEntries := b.N + 1
	if numEntries > maxEntriesNum {
		numEntries = maxEntriesNum
	}
	entries := mock.GenerateSeqEntries(ristrettoCfg, &repository.Backend{}, path, numEntries)

	for _, resp := range entries {
		store.Set(resp)
	}
	length := len(entries)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			for j := 0; j < 1000; j++ {
				_, _ = store.Get(entries[(i*j)%length])
			}
			i += 1000
		}
	})
	b.StopTimer()

	reportMemAndRistretto(b, store)
}

func BenchmarkRistrettoWrite1000x(b *testing.B) {
	store := NewRistrettoStorage(int64(ristrettoCfg.Cache.Storage.Size))
	numEntries := b.N + 1
	if numEntries > maxEntriesNum {
		numEntries = maxEntriesNum
	}
	entries := mock.GenerateSeqEntries(ristrettoCfg, &repository.Backend{}, path, numEntries)
	length := len(entries)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			for j := 0; j < 1000; j++ {
				store.Set(entries[(i*j)%length])
			}
			i += 1000
		}
	})
	b.StopTimer()

	reportMemAndRistretto(b, store)
}

func BenchmarkRistrettoGetAllocs(b *testing.B) {
	store := NewRistrettoStorage(int64(ristrettoCfg.Cache.Storage.Size))
	entry := mock.GenerateSeqEntries(ristrettoCfg, &repository.Backend{}, path, 1)[0]
	store.Set(entry)

	allocs := testing.AllocsPerRun(100_000, func() {
		_, _ = store.Get(entry)
	})
	b.ReportMetric(allocs, "allocs/op")

	reportMemAndRistretto(b, store)
}

func BenchmarkRistrettoSetAllocs(b *testing.B) {
	store := NewRistrettoStorage(int64(ristrettoCfg.Cache.Storage.Size))
	entry := mock.GenerateSeqEntries(ristrettoCfg, &repository.Backend{}, path, 1)[0]

	allocs := testing.AllocsPerRun(100_000, func() {
		store.Set(entry)
	})
	b.ReportMetric(allocs, "allocs/op")

	reportMemAndRistretto(b, store)
}
