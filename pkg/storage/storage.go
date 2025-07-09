package storage

import (
	"github.com/Borislavv/advanced-cache/pkg/model"
)

// Storage is a generic interface for cache storages.
// It supports typical Get/Set operations with reference management.
type Storage interface {
	// Run starts storage background worker (just logging at now).
	Run()

	// Get attempts to retrieve a cached response for the given request.
	// Returns the response, a releaser for safe concurrent access, and a hit/miss flag.
	Get(key, shard uint64) (resp *model.Response, isHit bool)

	// GetRandom attempts to retrieve any one cached response.
	GetRandom() (resp *model.Response, isFound bool)

	// Set stores a new response in the cache and returns a releaser for managing resource lifetime.
	Set(resp *model.Response)

	// Remove is removes one element.
	Remove(req *model.Response) (freedBytes int64, isHit bool)

	// Stat returns bytes usage and num of items in storage.
	Stat() (bytes int64, length int64)

	// Mem - return stored value (refreshes every 100ms).
	Mem() int64

	// RealMem - calculates and return value.
	RealMem() int64
}
