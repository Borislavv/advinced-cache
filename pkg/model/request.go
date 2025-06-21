package model

import (
	"bytes"
	"sort"
	"sync"
	"unsafe"

	"github.com/Borislavv/traefik-http-cache-plugin/pkg/config"
	sharded "github.com/Borislavv/traefik-http-cache-plugin/pkg/storage/map"
	"github.com/Borislavv/traefik-http-cache-plugin/pkg/synced"
	"github.com/Borislavv/traefik-http-cache-plugin/pkg/types"
	"github.com/valyala/fasthttp"
	"github.com/zeebo/xxh3"
)

const (
	defaultBufLen    = 56
	defaultPathLen   = 128
	defaultNumArgs   = 10
	defaultQueryLen  = 512
	defaultKeyBufLen = 1024
)

var (
	hasherPool   = &sync.Pool{New: func() any { return xxh3.New() }}
	requestsPool = synced.NewBatchPool[*Request](func() *Request {
		return new(Request)
	})
	queryBuffersPool = synced.NewBatchPool[*types.SizedBox[[]byte]](func() *types.SizedBox[[]byte] {
		return types.NewSizedBox[[]byte](make([]byte, 0, defaultQueryLen), func(s *types.SizedBox[[]byte]) int64 {
			return int64(len(s.Value))
		})
	})
	headerBuffersPool = synced.NewBatchPool[*types.SizedBox[[]byte]](func() *types.SizedBox[[]byte] {
		return types.NewSizedBox[[]byte](make([]byte, 0, defaultQueryLen), func(s *types.SizedBox[[]byte]) int64 {
			return int64(len(s.Value))
		})
	})
	keyBufferPool = synced.NewBatchPool[*types.SizedBox[[]byte]](func() *types.SizedBox[[]byte] {
		return types.NewSizedBox[[]byte](make([]byte, 0, defaultKeyBufLen), func(s *types.SizedBox[[]byte]) int64 {
			return int64(len(s.Value))
		})
	})
	pathBuffersPool = synced.NewBatchPool[*types.SizedBox[[]byte]](func() *types.SizedBox[[]byte] {
		return types.NewSizedBox[[]byte](make([]byte, 0, defaultPathLen), func(s *types.SizedBox[[]byte]) int64 {
			return int64(len(s.Value))
		})
	})
	sortedArgsAndHeadersPool = synced.NewBatchPool[*types.SizedBox[[]struct {
		Key   []byte
		Value []byte
	}]](func() *types.SizedBox[[]struct {
		Key   []byte
		Value []byte
	}] {
		return types.NewSizedBox[[]struct {
			Key   []byte
			Value []byte
		}](make([]struct {
			Key   []byte
			Value []byte
		}, 0, defaultNumArgs), func(s *types.SizedBox[[]struct {
			Key   []byte
			Value []byte
		}]) int64 {
			return int64(len(s.Value))
		})
	})
	sortedHeadersPool = synced.NewBatchPool[*types.SizedBox[[]struct {
		Key   []byte
		Value []byte
	}]](func() *types.SizedBox[[]struct {
		Key   []byte
		Value []byte
	}] {
		return types.NewSizedBox[[]struct {
			Key   []byte
			Value []byte
		}](make([]struct {
			Key   []byte
			Value []byte
		}, 0, defaultNumArgs), func(s *types.SizedBox[[]struct {
			Key   []byte
			Value []byte
		}]) int64 {
			return int64(len(s.Value))
		})
	})
	filterBuffersPool = synced.NewBatchPool[*types.SizedBox[*bytes.Buffer]](func() *types.SizedBox[*bytes.Buffer] {
		buf := new(bytes.Buffer)
		buf.Grow(defaultQueryLen)
		return types.NewSizedBox[*bytes.Buffer](buf, func(s *types.SizedBox[*bytes.Buffer]) int64 {
			return int64(s.Value.Cap())
		})
	})
	keyValueBufferPool = synced.NewBatchPool[*types.SizedBox[[]byte]](func() *types.SizedBox[[]byte] {
		return types.NewSizedBox[[]byte](make([]byte, 0, defaultBufLen), func(s *types.SizedBox[[]byte]) int64 {
			return int64(len(s.Value))
		})
	})
	filteredSlicesPool = synced.NewBatchPool[*types.SizedBox[[][2]*types.SizedBox[[]byte]]](func() *types.SizedBox[[][2]*types.SizedBox[[]byte]] {
		return types.NewSizedBox[[][2]*types.SizedBox[[]byte]](make([][2]*types.SizedBox[[]byte], 0, defaultBufLen), func(s *types.SizedBox[[][2]*types.SizedBox[[]byte]]) int64 {
			w := 0
			for _, v := range s.Value {
				w += cap(v[0].Value)
				w += cap(v[1].Value)
			}
			return int64(w)
		})
	})
	allowedQueryHeadersPool = synced.NewBatchPool[*types.SizedBox[[][]byte]](func() *types.SizedBox[[][]byte] {
		return types.NewSizedBox[[][]byte](make([][]byte, 0, defaultNumArgs), func(s *types.SizedBox[[][]byte]) int64 {
			w := int(unsafe.Sizeof(*s) + unsafe.Sizeof(s.Value))
			for _, v := range s.Value {
				w += len(v)
			}
			return int64(w)
		})
	})
)

type Request struct {
	cfg   *config.Cache
	key   uint64
	shard uint64
	args  *types.SizedBox[[]byte]
	path  *types.SizedBox[[]byte]
}

// NewRequestFromFasthttp - using in HOT PATH!
func NewRequestFromFasthttp(cfg *config.Cache, r *fasthttp.RequestCtx) *Request {
	sanitizeRequest(cfg, r)

	path := pathBuffersPool.Get()
	path.Value = append(path.Value, r.Path()...)

	req := requestsPool.Get()
	*req = Request{
		cfg:  cfg,
		path: path,
	}

	req.setUp(r.QueryArgs(), &r.Request.Header)

	return req
}

// NewRawRequest - using in dump loading.
func NewRawRequest(cfg *config.Cache, key, shard uint64, query, path []byte) *Request {
	argsBuf := queryBuffersPool.Get()
	argsBuf.Value = append(argsBuf.Value, query...)

	pathBuf := pathBuffersPool.Get()
	pathBuf.Value = append(pathBuf.Value, path...)

	req := requestsPool.Get()
	*req = Request{
		cfg:   cfg,
		key:   key,
		shard: shard,
		args:  argsBuf,
		path:  pathBuf,
	}

	return req
}

// NewTestRequest - using only in tests and benchmarks.
func NewTestRequest(cfg *config.Cache, path []byte, args map[string][]byte, headers map[string][][]byte) *Request {
	pathBuf := pathBuffersPool.Get()
	pathBuf.Value = append(pathBuf.Value, path...)

	req := requestsPool.Get()
	*req = Request{
		cfg:  cfg,
		path: pathBuf,
	}

	req.setUpManually(args, headers)

	return req
}

func (r *Request) ToQuery() []byte {
	return r.args.Value
}

func (r *Request) Path() []byte {
	return r.path.Value
}

func (r *Request) MapKey() uint64 {
	return r.key
}

func (r *Request) ShardKey() uint64 {
	return r.shard
}

func (r *Request) Weight() int64 {
	return int64(unsafe.Sizeof(*r)) + int64(len(r.args.Value))
}

func (r *Request) setUp(args *fasthttp.Args, header *fasthttp.RequestHeader) {
	argsBuf := queryBuffersPool.Get()

	if args.Len() > 0 {
		sortedArgs := sortedArgsAndHeadersPool.Get()
		defer func() {
			sortedArgs.Value = sortedArgs.Value[:0]
			sortedArgsAndHeadersPool.Put(sortedArgs)
		}()

		args.VisitAll(func(key, value []byte) {
			sortedArgs.Value = append(sortedArgs.Value, struct {
				Key   []byte
				Value []byte
			}{key, value})
		})

		sort.Slice(sortedArgs.Value, func(i, j int) bool {
			return bytes.Compare(sortedArgs.Value[i].Key, sortedArgs.Value[j].Key) < 0
		})

		argsBuf.Value = append(argsBuf.Value, '?')
		for _, arg := range sortedArgs.Value {
			argsBuf.Value = append(argsBuf.Value, arg.Key...)
			argsBuf.Value = append(argsBuf.Value, '=')
			argsBuf.Value = append(argsBuf.Value, arg.Value...)
			argsBuf.Value = append(argsBuf.Value, '&')
		}
		if len(argsBuf.Value) > 1 {
			argsBuf.Value = argsBuf.Value[:len(argsBuf.Value)-1] // убрать последний '&'
		} else {
			argsBuf.Value = argsBuf.Value[:0]
		}
	}
	r.args = argsBuf

	headersBuf := headerBuffersPool.Get()
	defer func() {
		headersBuf.Value = headersBuf.Value[:0]
		headerBuffersPool.Put(headersBuf)
	}()

	if header.Len() > 0 {
		sortedHeaders := sortedArgsAndHeadersPool.Get()
		defer func() {
			sortedHeaders.Value = sortedHeaders.Value[:0]
			sortedArgsAndHeadersPool.Put(sortedHeaders)
		}()
		header.VisitAll(func(key, value []byte) {
			sortedHeaders.Value = append(sortedHeaders.Value, struct {
				Key   []byte
				Value []byte
			}{key, value})
		})

		sort.Slice(sortedHeaders.Value, func(i, j int) bool {
			return bytes.Compare(sortedHeaders.Value[i].Key, sortedHeaders.Value[j].Key) < 0
		})

		for _, h := range sortedHeaders.Value {
			headersBuf.Value = append(headersBuf.Value, h.Key...)
			headersBuf.Value = append(headersBuf.Value, ':')
			headersBuf.Value = append(headersBuf.Value, h.Value...)
			headersBuf.Value = append(headersBuf.Value, '\n')
		}
		if len(headersBuf.Value) > 0 {
			headersBuf.Value = headersBuf.Value[:len(headersBuf.Value)-1] // убрать последний '\n'
		} else {
			headersBuf.Value = headersBuf.Value[:0]
		}
	}

	buf := keyBufferPool.Get()
	defer func() {
		buf.Value = buf.Value[:0]
		keyBufferPool.Put(buf)
	}()

	buf.Value = append(buf.Value, r.path.Value...)
	buf.Value = append(buf.Value, argsBuf.Value...)
	buf.Value = append(buf.Value, '\n')
	buf.Value = append(buf.Value, headersBuf.Value...)

	r.key = hash(buf.Value)
	r.shard = sharded.MapShardKey(r.key)
}

func (r *Request) setUpManually(args map[string][]byte, headers map[string][][]byte) {
	argsBuf := queryBuffersPool.Get()

	if len(args) > 0 {
		sortedArgs := sortedArgsAndHeadersPool.Get()
		defer func() {
			sortedArgs.Value = sortedArgs.Value[:0]
			sortedArgsAndHeadersPool.Put(sortedArgs)
		}()

		for key, value := range args {
			sortedArgs.Value = append(sortedArgs.Value, struct {
				Key   []byte
				Value []byte
			}{
				Key:   []byte(key), // allocation: at now this method does not use in hot path
				Value: value,
			})
		}

		sort.Slice(sortedArgs.Value, func(i, j int) bool {
			return bytes.Compare(sortedArgs.Value[i].Key, sortedArgs.Value[j].Key) < 0
		})

		argsBuf.Value = append(argsBuf.Value, '?')
		for _, arg := range sortedArgs.Value {
			argsBuf.Value = append(argsBuf.Value, arg.Key...)
			argsBuf.Value = append(argsBuf.Value, '=')
			argsBuf.Value = append(argsBuf.Value, arg.Value...)
			argsBuf.Value = append(argsBuf.Value, '&')
		}
		if len(argsBuf.Value) > 1 {
			argsBuf.Value = argsBuf.Value[:len(argsBuf.Value)-1]
		} else {
			argsBuf.Value = argsBuf.Value[:0]
		}
	}
	r.args = argsBuf

	headersBuf := headerBuffersPool.Get()
	defer func() {
		headersBuf.Value = headersBuf.Value[:0]
		headerBuffersPool.Put(headersBuf)
	}()

	if len(headers) > 0 {
		sortedHeaders := sortedHeadersPool.Get()
		defer func() {
			sortedHeaders.Value = sortedHeaders.Value[:0]
			sortedHeadersPool.Put(sortedHeaders)
		}()

		for key, values := range headers {
			for _, value := range values {
				sortedHeaders.Value = append(sortedHeaders.Value, struct {
					Key   []byte
					Value []byte
				}{
					Key:   []byte(key),
					Value: value,
				})
			}
		}

		sort.Slice(sortedHeaders.Value, func(i, j int) bool {
			return bytes.Compare(sortedHeaders.Value[i].Key, sortedHeaders.Value[j].Key) < 0
		})

		for _, h := range sortedHeaders.Value {
			headersBuf.Value = append(headersBuf.Value, h.Key...)
			headersBuf.Value = append(headersBuf.Value, ':')
			headersBuf.Value = append(headersBuf.Value, h.Value...)
			headersBuf.Value = append(headersBuf.Value, '\n')
		}
		if len(headersBuf.Value) > 0 {
			headersBuf.Value = headersBuf.Value[:len(headersBuf.Value)-1]
		} else {
			headersBuf.Value = headersBuf.Value[:0]
		}
	}

	buf := keyBufferPool.Get()
	defer func() {
		buf.Value = buf.Value[:0]
		keyBufferPool.Put(buf)
	}()

	buf.Value = append(buf.Value, r.path.Value...)
	buf.Value = append(buf.Value, argsBuf.Value...)
	buf.Value = append(buf.Value, '\n')
	buf.Value = append(buf.Value, headersBuf.Value...)

	r.key = hash(buf.Value)
	r.shard = sharded.MapShardKey(r.key)
}

func hash(buf []byte) uint64 {
	hasher := hasherPool.Get().(*xxh3.Hasher)
	defer hasherPool.Put(hasher)

	hasher.Reset()
	if _, err := hasher.Write(buf); err != nil {
		panic(err)
	}

	return hasher.Sum64()
}

func sanitizeRequest(cfg *config.Cache, ctx *fasthttp.RequestCtx) {
	allowedQueries, allowedHeaders := getKeyAllowed(cfg, ctx.Path())
	filterKeyQueriesInPlace(ctx, allowedQueries)
	filterKeyHeadersInPlace(ctx, allowedHeaders)
}

func getKeyAllowed(cfg *config.Cache, path []byte) (queries *types.SizedBox[[][]byte], headers *types.SizedBox[[][]byte]) {
	queries = allowedQueryHeadersPool.Get()
	headers = allowedQueryHeadersPool.Get()
	for _, rule := range cfg.Cache.Rules {
		if bytes.HasPrefix(path, rule.PathBytes) {
			for _, param := range rule.CacheKey.QueryBytes {
				queries.Value = append(queries.Value, param)
			}
			for _, header := range rule.CacheKey.HeadersBytes {
				headers.Value = append(headers.Value, header)
			}
		}
	}
	return queries, headers
}

func filterKeyQueriesInPlace(ctx *fasthttp.RequestCtx, allowed *types.SizedBox[[][]byte]) {
	defer func() {
		for key, _ := range allowed.Value {
			allowed.Value[key] = allowed.Value[key][:0]
		}
		allowed.Value = allowed.Value[:0]
		allowedQueryHeadersPool.Put(allowed)
	}()

	original := ctx.QueryArgs()
	buf := filterBuffersPool.Get()
	defer func() {
		buf.Value.Reset()
		filterBuffersPool.Put(buf)
	}()

	original.VisitAll(func(k, v []byte) {
		for _, ak := range allowed.Value {
			if bytes.HasPrefix(k, ak) {
				buf.Value.Write(k)
				buf.Value.WriteByte('=')
				buf.Value.Write(v)
				buf.Value.WriteByte('&')
				break
			}
		}
	})

	if buf.Value.Len() > 0 {
		buf.Value.Truncate(buf.Value.Len() - 1) // remove the last &
		ctx.URI().SetQueryStringBytes(buf.Value.Bytes())
	} else {
		ctx.URI().SetQueryStringBytes(nil) // remove query
	}
}

func filterKeyHeadersInPlace(ctx *fasthttp.RequestCtx, allowed *types.SizedBox[[][]byte]) {
	defer func() {
		for key, _ := range allowed.Value {
			allowed.Value[key] = allowed.Value[key][:0]
		}
		allowed.Value = allowed.Value[:0]
		allowedQueryHeadersPool.Put(allowed)
	}()

	headers := &ctx.Request.Header

	var filteredSlice *types.SizedBox[[][2]*types.SizedBox[[]byte]]
	headers.VisitAll(func(k, v []byte) {
		for _, ak := range allowed.Value {
			if bytes.EqualFold(k, ak) {
				// allocate a new slice because the origin slice valid until request is alive,
				// further this "value" (slice) will be reused for new data be fasthttp (owner).
				// Don't remove allocation or will have UNDEFINED BEHAVIOR!

				keyCopied := keyValueBufferPool.Get()
				keyCopied.Value = append(keyCopied.Value, k...)

				valueCopied := keyValueBufferPool.Get()
				valueCopied.Value = append(valueCopied.Value, v...)

				filteredSlice = filteredSlicesPool.Get()
				filteredSlice.Value = append(filteredSlice.Value, [2]*types.SizedBox[[]byte]{keyCopied, valueCopied})

				break
			}
		}
	})

	// Remove all headers
	headers.Reset()

	if filteredSlice != nil {
		// Setting up only allowed
		for _, filtered := range filteredSlice.Value {
			headers.SetBytesKV(filtered[0].Value, filtered[1].Value)

			filtered[0].Value = filtered[0].Value[:0]
			keyValueBufferPool.Put(filtered[0])

			filtered[1].Value = filtered[1].Value[:0]
			keyValueBufferPool.Put(filtered[1])
		}

		filteredSlice.Value = filteredSlice.Value[:0]
		filteredSlicesPool.Put(filteredSlice)
	}
}

// clear resets the Request to zero (except for buffer capacity).
func (r *Request) clear() *Request {
	r.key = 0
	r.shard = 0
	r.args = nil
	r.path = nil
	return r
}

// Release releases and resets the request (and any underlying buffer/tag slices) for reuse.
func (r *Request) Release() {
	r.args.Value = r.args.Value[:0] // reset slice len
	queryBuffersPool.Put(r.args)    // release args buffer

	r.path.Value = r.path.Value[:0] // reset slice len
	pathBuffersPool.Put(r.path)     // release args path buffer

	r.clear()           // clear itself request (set up zero-values)
	requestsPool.Put(r) // release itself request
}
