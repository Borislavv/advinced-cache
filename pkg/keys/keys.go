package keys

import (
	"bytes"
	"github.com/Borislavv/advanced-cache/pkg/config"
	"github.com/Borislavv/advanced-cache/pkg/intern"
	sharded "github.com/Borislavv/advanced-cache/pkg/storage/map"
	"github.com/valyala/fasthttp"
	"github.com/zeebo/xxh3"
	"sort"
	"sync"
)

var (
	bufPool = &sync.Pool{
		New: func() interface{} {
			return new(bytes.Buffer)
		},
	}

	hasherPool = &sync.Pool{
		New: func() any {
			return xxh3.New()
		},
	}
)

func Calculate(path []byte, queries [][2][]byte, headers [][2][]byte) (key uint64, shard uint64) {
	argsLength := 0
	for _, pair := range queries {
		argsLength += len(pair[0]) + len(pair[1])
	}
	headersLength := 0
	for _, pair := range headers {
		headersLength += len(pair[0]) + len(pair[1])
	}

	buf := bufPool.Get().(*bytes.Buffer)
	defer func() {
		buf.Reset()
		bufPool.Put(buf)
	}()
	buf.Grow(len(path) + argsLength + headersLength)

	buf.Write(path)
	for _, pair := range queries {
		buf.Write(pair[0])
		buf.Write(pair[1])
	}
	for _, pair := range headers {
		buf.Write(pair[0])
		buf.Write(pair[1])
	}

	key = hash(buf)
	shard = sharded.MapShardKey(key)

	return
}

var (
	filteredQueryPool = &sync.Pool{
		New: func() interface{} {
			return make([][2][]byte, 0, 8)
		},
	}
	filteredHeaderPool = &sync.Pool{
		New: func() interface{} {
			return make([][2][]byte, 0, 8)
		},
	}
	sliceBytesPool = &sync.Pool{
		New: func() interface{} {
			return make([]byte, 0, 32)
		},
	}
)

func CalculateFasthttp(rule *config.Rule, internedPath []byte, r *fasthttp.RequestCtx) (key uint64, shard uint64) {
	var filteredQueries = filteredQueryPool.Get().([][2][]byte)
	defer func() {
		for _, kv := range filteredQueries {
			kv[1] = kv[1][:0]
			sliceBytesPool.Put(kv[1])
		}
		filteredQueries = filteredQueries[:0]
		filteredQueryPool.Put(filteredQueries)
	}()

	queriesLength := 0
	if len(rule.CacheKey.QueryBytes) > 0 {
		r.QueryArgs().All()(func(k, v []byte) bool {
			for _, ak := range rule.CacheKey.QueryBytes {
				if bytes.HasPrefix(k, ak) {
					internedKey := intern.QueryKeyInterner.Intern(k, true)

					valueBuf := sliceBytesPool.Get().([]byte)
					valueBuf = append(valueBuf, v...)

					// NOTE: safe copy for value
					filteredQueries = append(filteredQueries, [2][]byte{internedKey, valueBuf})
					queriesLength += len(k) + len(v)
					break
				}
			}
			return true
		})

		// Sort in place
		sort.Slice(filteredQueries, func(i, j int) bool {
			return bytes.Compare(filteredQueries[i][0], filteredQueries[j][0]) < 0
		})
	}

	var filteredHeaders = filteredHeaderPool.Get().([][2][]byte)
	defer func() {
		for _, kv := range filteredHeaders {
			kv[1] = kv[1][:0]
			sliceBytesPool.Put(kv[1])
		}
		filteredHeaders = filteredHeaders[:0]
		filteredHeaderPool.Put(filteredHeaders)
	}()

	headersLength := 0
	if len(rule.CacheKey.HeadersBytes) > 0 {
		r.Request.Header.All()(func(k, v []byte) bool {
			for _, ak := range rule.CacheKey.HeadersBytes {
				if bytes.HasPrefix(k, ak) {
					internedKey := intern.HeaderKeyInterner.Intern(k, true)

					valueBuf := sliceBytesPool.Get().([]byte)
					valueBuf = append(valueBuf, v...)

					// NOTE: safe copy for value
					filteredHeaders = append(filteredHeaders, [2][]byte{internedKey, valueBuf})
					headersLength += len(k) + len(v)
					break
				}
			}
			return true
		})

		// Sort in place
		sort.Slice(filteredHeaders, func(i, j int) bool {
			return bytes.Compare(filteredHeaders[i][0], filteredHeaders[j][0]) < 0
		})
	}

	buf := bufPool.Get().(*bytes.Buffer)
	defer func() {
		buf.Reset()
		bufPool.Put(buf)
	}()
	buf.Grow(len(internedPath) + queriesLength + headersLength)

	buf.Write(internedPath)
	for _, pair := range filteredQueries {
		buf.Write(pair[0])
		buf.Write(pair[1])
	}
	for _, pair := range filteredHeaders {
		buf.Write(pair[0])
		buf.Write(pair[1])
	}

	key = hash(buf)
	shard = sharded.MapShardKey(key)

	return
}

func hash(buf *bytes.Buffer) uint64 {
	hasher := hasherPool.Get().(*xxh3.Hasher)
	defer func() {
		hasher.Reset()
		hasherPool.Put(hasher)
	}()

	if _, err := hasher.Write(buf.Bytes()); err != nil {
		panic(err)
	}

	return hasher.Sum64()
}
