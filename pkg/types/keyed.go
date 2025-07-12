package types

// Keyed defines a unique key and a precomputed shard key for the value.
type Keyed interface {
	MapKey() uint64
	ShardKey() uint64
	Fingerprint() [16]byte
	IsSameFingerprint([16]byte) bool
}
