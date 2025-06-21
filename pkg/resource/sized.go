package resource

// Sized provides memory usage accounting for cache entries.
type Sized interface {
	Weight() int64
}
