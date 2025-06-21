package resource

// Keyed defines a unique key and a precomputed shard key for the value.
type Keyed interface {
	MapKey() uint64
	ShardKey() uint64
}
