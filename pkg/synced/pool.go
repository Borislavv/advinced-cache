package synced

import (
	"github.com/Borislavv/traefik-http-cache-plugin/pkg/resource"
	"sync"
)

// BatchPool is a high-throughput generic object pool with batch preallocation.
//
// The main goal is to:
// - Minimize allocations by reusing objects.
// - Reduce allocation spikes by preallocating objects in large batches.
// - Provide simple Get/Put API similar to sync.Pool but with better bulk allocation behavior.
type BatchPool[T resource.Sized] struct {
	pool      *sync.Pool // Underlying sync.Pool for thread-safe pooling
	allocFunc func() T   // Function to create new T
}

// NewBatchPool creates a new BatchPool with an initial preallocation.
// - preallocateBatchSize: how many objects to add to the pool per allocation batch.
// - allocFunc: function to construct a new T.
func NewBatchPool[T resource.Sized](allocFunc func() T) *BatchPool[T] {
	bp := &BatchPool[T]{allocFunc: allocFunc}
	bp.pool = &sync.Pool{
		New: func() any {
			return allocFunc()
		},
	}

	return bp
}

// Get retrieves an object from the pool, allocating if necessary.
// Never returns nil (unless allocFunc does).
func (bp *BatchPool[T]) Get() T {
	return bp.pool.Get().(T)
}

// Put returns an object to the pool for future reuse.
func (bp *BatchPool[T]) Put(v T) {
	bp.pool.Put(v)
}
