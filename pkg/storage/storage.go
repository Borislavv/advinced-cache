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
	Get(*model.Entry) (entry *model.Entry, isHit bool)

	// GetRand returns a random elem from the map.
	GetRand() (*model.Entry, bool)

	// Set stores a new response in the cache and returns a releaser for managing resource lifetime.
	Set(entry *model.Entry)

	// Remove is removes one element.
	Remove(req *model.Entry) (freedBytes int64, isHit bool)

	// Stat returns bytes usage and num of items in storage.
	Stat() (bytes int64, length int64)

	// Len - return stored value (refreshes every 100ms).
	Len() int64

	// Mem - return stored value (refreshes every 100ms).
	Mem() int64

	// RealMem - calculates and return value.
	RealMem() int64
}
