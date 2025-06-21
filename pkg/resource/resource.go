package resource

// Resource must implement all cache entry interfaces: keying, sizing, and releasability.
type Resource interface {
	Keyed
	Sized
	Releasable
}
