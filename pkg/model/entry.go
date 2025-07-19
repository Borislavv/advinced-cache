package model

import (
	"bytes"
	"compress/gzip"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/Borislavv/advanced-cache/pkg/config"
	"github.com/Borislavv/advanced-cache/pkg/list"
	"github.com/Borislavv/advanced-cache/pkg/pools"
	"github.com/Borislavv/advanced-cache/pkg/repository"
	sharded "github.com/Borislavv/advanced-cache/pkg/storage/map"
	"github.com/rs/zerolog/log"
	"github.com/valyala/fasthttp"
	"github.com/zeebo/xxh3"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

var (
	entriesPool       = sync.Pool{New: func() interface{} { return new(Entry).Init() }}
	bufPool           = &sync.Pool{New: func() any { return new(bytes.Buffer) }}
	hasherPool        = &sync.Pool{New: func() any { return xxh3.New() }}
	ruleNotFoundError = errors.New("rule not found")
)

// Entry is the packed request+response payload
type Entry struct {
	key          uint64   // 64  bit xxh
	shard        uint64   // 64  bit xxh % NumOfShards
	fingerprint  [16]byte // 128 bit xxh
	rule         *config.Rule
	payload      *atomic.Pointer[[]byte]
	lruListElem  *atomic.Pointer[list.Element[*VersionPointer]]
	revalidator  Revalidator
	willUpdateAt int64  // atomic: unix nano
	isCompressed int64  // atomic: bool as int64
	isDoomed     int64  // atomic: bool as int64
	refCount     int64  // atomic: simple counter
	version      uint64 // atomic: solves ABA problem (pattern: version pointers)
	// (when you already changed an object in storage but someone just now try to do Acquire on already finalized and reused object)
}

func (e *Entry) Init() *Entry {
	e.payload = &atomic.Pointer[[]byte]{}
	e.lruListElem = &atomic.Pointer[list.Element[*VersionPointer]]{}
	return e
}

func drainEntryPool() *Entry {
	e := entriesPool.Get().(*Entry)
	atomic.StoreInt64(&e.isDoomed, 0)
	atomic.StoreInt64(&e.refCount, 1)
	return e
}

// NewEntryNetHttp accepts path, query and request headers as bytes slices.
func NewEntryNetHttp(cfg *config.Cache, r *http.Request) (*Entry, error) {
	// path is a string in net/http so easily refer to it inside request
	rule := MatchRuleStr(cfg, r.URL.Path)
	if rule == nil {
		return nil, ruleNotFoundError
	}

	entry := drainEntryPool()
	entry.rule = rule

	filteredQueries, queriesReleaser := entry.GetFilteredAndSortedKeyQueriesNetHttp(r)
	defer queriesReleaser()

	filteredHeaders, headersReleaser := entry.GetFilteredAndSortedKeyHeadersNetHttp(r)
	defer headersReleaser()

	return entry.calculateAndSetUpKeys(filteredQueries, filteredHeaders), nil
}

// NewEntryFastHttp accepts path, query and request headers as bytes slices.
func NewEntryFastHttp(cfg *config.Cache, r *fasthttp.RequestCtx) (*Entry, error) {
	rule := MatchRule(cfg, r.Path())
	if rule == nil {
		return nil, ruleNotFoundError
	}

	entry := drainEntryPool()
	entry.rule = rule

	filteredQueries, queriesReleaser := entry.getFilteredAndSortedKeyQueriesFastHttp(r)
	defer queriesReleaser()

	filteredHeaders, headersReleaser := entry.getFilteredAndSortedKeyHeadersFastHttp(r)
	defer headersReleaser()

	return entry.calculateAndSetUpKeys(filteredQueries, filteredHeaders), nil
}

func NewEntryManual(cfg *config.Cache, path, query []byte, headers [][2][]byte, revalidator Revalidator) (*Entry, error) {
	rule := MatchRule(cfg, path)
	if rule == nil {
		return nil, ruleNotFoundError
	}

	entry := drainEntryPool()
	entry.rule = rule
	entry.revalidator = revalidator

	filteredQueries, queriesReleaser := entry.parseFilterAndSortQuery(query) // here, we are referring to the same query buffer which used in payload which have been mentioned before
	defer queriesReleaser()                                                  // this is really reduce memory usage and GC pressure

	sort.Slice(filteredQueries, func(i, j int) bool {
		return bytes.Compare(filteredQueries[i][0], filteredQueries[j][0]) < 0
	})

	filteredHeaders := entry.filteredAndSortedKeyHeadersInPlace(headers)

	return entry.calculateAndSetUpKeys(filteredQueries, filteredHeaders), nil
}

func NewEntryFromField(
	key uint64,
	shard uint64,
	fingerprint [16]byte,
	payload []byte,
	rule *config.Rule,
	revalidator Revalidator,
	isCompressed int64,
	willUpdateAt int64,
) *Entry {
	entry := drainEntryPool()
	entry.key = key
	entry.shard = shard
	entry.fingerprint = fingerprint
	entry.rule = rule
	entry.payload.Store(&payload)
	entry.revalidator = revalidator
	entry.isCompressed = isCompressed
	entry.willUpdateAt = willUpdateAt
	return entry
}

func (e *Entry) MapKey() uint64   { return e.key }
func (e *Entry) ShardKey() uint64 { return e.shard }

const (
	// refCount values
	preDoomedCount = 1
	zeroRefCount   = 0
	doomedRefCount = -1
	// isDoomed values
	notDoomed = 0
	doomed    = 1
)

func (e *Entry) Acquire(expectedVersion uint64) bool {
	if e == nil {
		return false
	}
	for {
		curVersion := atomic.LoadUint64(&e.version)
		if curVersion != expectedVersion {
			return false
		}
		ref := atomic.LoadInt64(&e.refCount)
		if ref < 0 {
			return false
		}
		if atomic.CompareAndSwapInt64(&e.refCount, ref, ref+1) {
			// double-check Version to catch finalize()
			if atomic.LoadUint64(&e.version) != expectedVersion {
				e.Release()
				return false
			}
			return true
		}
	}
}

func (e *Entry) Release() {
	if e == nil {
		return
	}
	for {
		if old := atomic.LoadInt64(&e.refCount); atomic.CompareAndSwapInt64(&e.refCount, old, old-1) {
			if old == preDoomedCount && atomic.LoadInt64(&e.isDoomed) == doomed {
				if atomic.CompareAndSwapInt64(&e.refCount, zeroRefCount, doomedRefCount) {
					e.finalize()
					return
				}
			}
			return
		}
	}
}

func (e *Entry) Remove() {
	if e == nil {
		return
	}
	if atomic.CompareAndSwapInt64(&e.isDoomed, notDoomed, doomed) {
		e.Release()
		return
	}
	return
}

// finalize - after this action you cannot use this Entry!
// if you will, you will receive errors which are extremely hard-debug like "nil pointer dereference" and "non-consistent data into entry"
// due to the Entry which was returned into pool will be used again in other threads.
func (e *Entry) finalize() (freedMem int64) {
	atomic.AddUint64(&e.version, 1)

	e.key = 0
	e.shard = 0
	e.rule = nil
	e.willUpdateAt = 0
	e.isCompressed = 0
	e.revalidator = nil
	e.lruListElem.Store(nil)
	e.payload.Store(nil)
	freedMem = e.Weight()

	// return back to pool
	entriesPool.Put(e)

	return
}

func (e *Entry) Version() uint64 {
	return atomic.LoadUint64(&e.version)
}

var keyBufPool = sync.Pool{
	New: func() interface{} {
		return new(bytes.Buffer)
	},
}

func (e *Entry) calculateAndSetUpKeys(filteredQueries, filteredHeaders [][2][]byte) *Entry {
	l := 0
	for _, pair := range filteredQueries {
		l += len(pair[0]) + len(pair[1])
	}
	for _, pair := range filteredHeaders {
		l += len(pair[0]) + len(pair[1])
	}

	buf := keyBufPool.Get().(*bytes.Buffer)
	buf.Grow(l)
	defer func() {
		buf.Reset()
		keyBufPool.Put(buf)
	}()

	for _, pair := range filteredQueries {
		buf.Write(pair[0])
		buf.Write(pair[1])
	}
	for _, pair := range filteredHeaders {
		buf.Write(pair[0])
		buf.Write(pair[1])
	}

	hasher := hasherPool.Get().(*xxh3.Hasher)
	defer func() {
		hasher.Reset()
		hasherPool.Put(hasher)
	}()

	// calculate key hash
	if _, err := hasher.Write(buf.Bytes()); err != nil {
		panic(err)
	}
	e.key = hasher.Sum64()

	// calculate fingerprint hash
	fp := hasher.Sum128()
	var fingerprint [16]byte
	binary.LittleEndian.PutUint64(fingerprint[0:8], fp.Lo)
	binary.LittleEndian.PutUint64(fingerprint[8:16], fp.Hi)
	e.fingerprint = fingerprint

	// calculate shard index
	e.shard = sharded.MapShardKey(e.key)

	return e
}

func (e *Entry) Fingerprint() [16]byte {
	return e.fingerprint
}

func (e *Entry) IsSameFingerprint(another [16]byte) bool {
	return subtle.ConstantTimeCompare(e.fingerprint[:], another[:]) == 1
}

func (e *Entry) IsSameEntry(another *Entry) bool {
	return subtle.ConstantTimeCompare(e.fingerprint[:], another.fingerprint[:]) == 1 &&
		e.isPayloadsAreEquals(e.PayloadBytes(), another.PayloadBytes())
}

func (e *Entry) SetRevalidator(revalidator Revalidator) {
	e.revalidator = revalidator
}

func (e *Entry) isPayloadsAreEquals(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) < 32 {
		return bytes.Equal(a, b)
	}

	ha := xxh3.Hash(a[:8]) ^ xxh3.Hash(a[len(a)/2:len(a)/2+8]) ^ xxh3.Hash(a[len(a)-8:])
	hb := xxh3.Hash(b[:8]) ^ xxh3.Hash(b[len(b)/2:len(b)/2+8]) ^ xxh3.Hash(b[len(b)-8:])
	return ha == hb
}

// SetPayload packs and gzip-compresses the entire payload: Path, Query, QueryHeaders, StatusCode, ResponseHeaders, Body.
func (e *Entry) SetPayload(
	path, query []byte,
	queryHeaders [][2][]byte,
	headers [][2][]byte,
	body []byte,
	status int,
) {
	numQueryHeaders := len(queryHeaders)
	numResponseHeaders := len(headers)

	// === 1) Calculate total size ===
	total := 0
	total += 4 + len(path)
	total += 4 + len(query)
	total += 4
	for _, kv := range queryHeaders {
		total += 4 + len(kv[0]) + 4 + len(kv[1])
	}
	total += 4
	total += 4
	for _, kv := range headers {
		total += 4 + len(kv[0]) + 4 + 4 + len(kv[1])
	}
	total += 4 + len(body)

	// === 2) Allocate ===
	payloadBuf := make([]byte, 0, total)
	offset := 0

	var scratch [4]byte

	// === 3) Write ===

	// Path

	binary.LittleEndian.PutUint32(scratch[:], uint32(len(path)))
	payloadBuf = append(payloadBuf, scratch[:]...)
	payloadBuf = append(payloadBuf, path...)
	offset += len(path)

	// Query
	binary.LittleEndian.PutUint32(scratch[:], uint32(len(query)))
	payloadBuf = append(payloadBuf, scratch[:]...)
	payloadBuf = append(payloadBuf, query...)
	offset += len(query)

	// QueryHeaders
	binary.LittleEndian.PutUint32(scratch[:], uint32(numQueryHeaders))
	payloadBuf = append(payloadBuf, scratch[:]...)
	for _, kv := range queryHeaders {
		binary.LittleEndian.PutUint32(scratch[:], uint32(len(kv[0])))
		payloadBuf = append(payloadBuf, scratch[:]...)
		payloadBuf = append(payloadBuf, kv[0]...)
		offset += len(kv[0])

		binary.LittleEndian.PutUint32(scratch[:], uint32(len(kv[1])))
		payloadBuf = append(payloadBuf, scratch[:]...)
		payloadBuf = append(payloadBuf, kv[1]...)
		offset += len(kv[1])
	}

	// StatusCode
	binary.LittleEndian.PutUint32(scratch[:], uint32(status))
	payloadBuf = append(payloadBuf, scratch[:]...)

	// ResponseHeaders
	binary.LittleEndian.PutUint32(scratch[:], uint32(numResponseHeaders))
	payloadBuf = append(payloadBuf, scratch[:]...)
	for _, kv := range headers {
		binary.LittleEndian.PutUint32(scratch[:], uint32(len(kv[0])))
		payloadBuf = append(payloadBuf, scratch[:]...)
		payloadBuf = append(payloadBuf, kv[0]...)
		offset += len(kv[0])

		binary.LittleEndian.PutUint32(scratch[:], uint32(1))
		payloadBuf = append(payloadBuf, scratch[:]...)
		binary.LittleEndian.PutUint32(scratch[:], uint32(len(kv[1])))
		payloadBuf = append(payloadBuf, scratch[:]...)
		payloadBuf = append(payloadBuf, kv[1]...)
		offset += len(kv[1])
	}

	// Body
	binary.LittleEndian.PutUint32(scratch[:], uint32(len(body)))
	payloadBuf = append(payloadBuf, scratch[:]...)
	payloadBuf = append(payloadBuf, body...)
	offset += len(body)

	// === 4) Compress if needed ===
	if e.rule.Gzip.Enabled && total >= e.rule.Gzip.Threshold {
		var compressed bytes.Buffer
		gzipWriter := gzip.NewWriter(&compressed)
		_, err := gzipWriter.Write(payloadBuf)
		closeErr := gzipWriter.Close()
		if err == nil && closeErr == nil {
			bufCopy := make([]byte, compressed.Len())
			copy(bufCopy, compressed.Bytes())
			e.payload.Store(&bufCopy)
			atomic.StoreInt64(&e.isCompressed, 1)
			return
		}
	}

	// === 5) Store raw ===
	payloadBuf = payloadBuf[:]
	e.payload.Store(&payloadBuf)
	atomic.StoreInt64(&e.isCompressed, 0)
}

// Payload decompresses the entire payload and unpacks it into fields.
func (e *Entry) Payload() (
	path []byte,
	query []byte,
	queryHeaders [][2][]byte,
	responseHeaders [][2][]byte,
	body []byte,
	status int,
	err error,
) {
	payload := e.PayloadBytes()
	if len(payload) == 0 {
		return nil, nil, nil, nil, nil, 0, fmt.Errorf("payload is empty")
	}

	var rawPayload []byte
	if atomic.LoadInt64(&e.isCompressed) == 1 {
		// TODO need to move it to sync.Pool
		gr, err := gzip.NewReader(bytes.NewReader(payload))
		if err != nil {
			return nil, nil, nil, nil, nil, 0, fmt.Errorf("make gzip reader error: %w", err)
		}
		defer gr.Close()

		rawPayload, err = io.ReadAll(gr)
		if err != nil {
			return nil, nil, nil, nil, nil, 0, fmt.Errorf("gzip read failed: %w", err)
		}
	} else {
		rawPayload = payload
	}

	offset := 0

	// --- Path
	pathLen := binary.LittleEndian.Uint32(rawPayload[offset:])
	offset += 4
	path = rawPayload[offset : offset+int(pathLen)]
	offset += int(pathLen)

	// --- Query
	queryLen := binary.LittleEndian.Uint32(rawPayload[offset:])
	offset += 4
	query = rawPayload[offset : offset+int(queryLen)]
	offset += int(queryLen)

	// --- QueryHeaders
	numQueryHeaders := binary.LittleEndian.Uint32(rawPayload[offset:])
	offset += 4
	queryHeaders = pools.KeyValueSlicePool.Get().([][2][]byte)
	for i := 0; i < int(numQueryHeaders); i++ {
		keyLen := binary.LittleEndian.Uint32(rawPayload[offset:])
		offset += 4
		k := rawPayload[offset : offset+int(keyLen)]
		offset += int(keyLen)

		valueLen := binary.LittleEndian.Uint32(rawPayload[offset:])
		offset += 4
		v := rawPayload[offset : offset+int(valueLen)]
		offset += int(valueLen)

		queryHeaders = append(queryHeaders, [2][]byte{k, v})
	}

	// --- StatusCode
	status = int(binary.LittleEndian.Uint32(rawPayload[offset:]))
	offset += 4

	// --- Response Headers
	numHeaders := binary.LittleEndian.Uint32(rawPayload[offset:])
	offset += 4
	responseHeaders = pools.KeyValueSlicePool.Get().([][2][]byte)
	for i := 0; i < int(numHeaders); i++ {
		keyLen := binary.LittleEndian.Uint32(rawPayload[offset:])
		offset += 4
		key := rawPayload[offset : offset+int(keyLen)]
		offset += int(keyLen)

		numVals := binary.LittleEndian.Uint32(rawPayload[offset:])
		offset += 4
		for v := 0; v < int(numVals); v++ {
			valueLen := binary.LittleEndian.Uint32(rawPayload[offset:])
			offset += 4
			val := rawPayload[offset : offset+int(valueLen)]
			offset += int(valueLen)
			responseHeaders = append(responseHeaders, [2][]byte{key, val})
		}
	}

	// --- Body
	offset += 4
	body = rawPayload[offset:]

	return
}

func (e *Entry) ReleasePayload(queryHeaders, responseHeaders [][2][]byte) {
	queryHeaders = queryHeaders[:0]
	pools.KeyValueSlicePool.Put(queryHeaders)

	responseHeaders = responseHeaders[:0]
	pools.KeyValueSlicePool.Put(responseHeaders)
}

func (e *Entry) Rule() *config.Rule {
	return e.rule
}

func (e *Entry) PayloadBytes() []byte {
	var payload []byte
	ptr := e.payload.Load()
	if ptr != nil {
		return *ptr
	}
	return payload
}

func (e *Entry) Weight() int64 {
	return int64(unsafe.Sizeof(*e)) + int64(cap(e.PayloadBytes()))
}

func (e *Entry) IsCompressed() bool {
	return atomic.LoadInt64(&e.isCompressed) == 1
}

func (e *Entry) WillUpdateAt() int64 {
	return atomic.LoadInt64(&e.willUpdateAt)
}

func (e *Entry) parseFilterAndSortQuery(b []byte) (queries [][2][]byte, releaseFn func()) {
	b = bytes.TrimLeft(b, "?")

	queries = pools.KeyValueSlicePool.Get().([][2][]byte)
	queries = queries[:0]

	type state struct {
		kIdx   int
		vIdx   int
		kFound bool
		vFound bool
	}

	// parsing
	s := state{}
	for idx, bt := range b {
		if bt == '&' {
			if s.kFound {
				var key, val []byte
				if s.vFound {
					key = b[s.kIdx : s.vIdx-1]
					val = b[s.vIdx:idx]
				} else {
					key = b[s.kIdx:idx]
					val = []byte{}
				}
				queries = append(queries, [2][]byte{key, val})
			}
			s.kIdx = idx + 1
			s.kFound = true
			s.vIdx = 0
			s.vFound = false
		} else if bt == '=' && !s.vFound {
			s.vIdx = idx + 1
			s.vFound = true
		} else if !s.kFound {
			s.kIdx = idx
			s.kFound = true
		}
	}

	if s.kFound {
		var key, val []byte
		if s.vFound {
			key = b[s.kIdx : s.vIdx-1]
			val = b[s.vIdx:]
		} else {
			key = b[s.kIdx:]
			val = []byte{}
		}
		queries = append(queries, [2][]byte{key, val})
	}

	// filtering
	filtered := queries[:0]
	for _, kv := range queries {
		keep := false
		for _, allowedKey := range e.rule.CacheKey.QueryBytes {
			if bytes.HasPrefix(kv[0], allowedKey) {
				keep = true
				break
			}
		}
		if keep {
			filtered = append(filtered, kv)
		}
	}
	queries = filtered

	if len(queries) > 1 {
		sort.Slice(queries, func(i, j int) bool {
			return bytes.Compare(queries[i][0], queries[j][0]) < 0
		})
	}

	return queries, func() {
		queries = queries[:0]
		pools.KeyValueSlicePool.Put(queries)
	}
}

var kvPool = sync.Pool{
	New: func() interface{} {
		return make([][2][]byte, 0, 32)
	},
}

func (e *Entry) getFilteredAndSortedKeyQueriesFastHttp(r *fasthttp.RequestCtx) (kvPairs [][2][]byte, releaseFn func()) {
	filtered := kvPool.Get().([][2][]byte)
	filtered = filtered[:0]

	allowedKeys := e.rule.CacheKey.QueryBytes

	r.QueryArgs().All()(func(key, value []byte) bool {
		allowed := false
		for _, ak := range allowedKeys {
			if bytes.HasPrefix(key, ak) {
				allowed = true
				break
			}
		}
		if !allowed {
			return true
		}
		filtered = append(filtered, [2][]byte{key, value})
		return true
	})

	sort.Slice(filtered, func(i, j int) bool {
		return bytes.Compare(filtered[i][0], filtered[j][0]) < 0
	})

	return filtered, func() {
		filtered = filtered[:0]
		kvPool.Put(filtered)
	}
}

func (e *Entry) GetFilteredAndSortedKeyQueriesNetHttp(r *http.Request) (kvPairs [][2][]byte, releaseFn func()) {
	// r.URL.RawQuery - is static immutable string, therefor we can easily refer to it without any allocations.
	return e.parseFilterAndSortQuery(unsafe.Slice(unsafe.StringData(r.URL.RawQuery), len(r.URL.RawQuery)))
}

var hKvPool = sync.Pool{
	New: func() interface{} {
		return make([][2][]byte, 0, 32)
	},
}

func (e *Entry) getFilteredAndSortedKeyHeadersFastHttp(r *fasthttp.RequestCtx) (kvPairs [][2][]byte, releaseFn func()) {
	filtered := hKvPool.Get().([][2][]byte)
	allowedKeysMap := e.rule.CacheKey.HeadersMap

	r.Request.Header.All()(func(key, value []byte) bool {
		if _, ok := allowedKeysMap[unsafe.String(unsafe.SliceData(key), len(key))]; ok {
			filtered = append(filtered, [2][]byte{key, value})
		}
		return true
	})

	sort.Slice(filtered, func(i, j int) bool {
		return bytes.Compare(filtered[i][0], filtered[j][0]) < 0
	})

	return filtered, func() {
		filtered = filtered[:0]
		hKvPool.Put(filtered)
	}
}

func (e *Entry) GetFilteredAndSortedKeyHeadersNetHttp(r *http.Request) (kvPairs [][2][]byte, releaseFn func()) {
	filtered := hKvPool.Get().([][2][]byte)
	allowedKeysMap := e.rule.CacheKey.HeadersMap

	for key, vv := range r.Header {
		keyBytes := unsafe.Slice(unsafe.StringData(key), len(key))

		if _, ok := allowedKeysMap[key]; ok {
			for _, value := range vv {
				valBytes := unsafe.Slice(unsafe.StringData(value), len(value))
				filtered = append(filtered, [2][]byte{keyBytes, valBytes})
			}
		}
	}

	sort.Slice(filtered, func(i, j int) bool {
		return bytes.Compare(filtered[i][0], filtered[j][0]) < 0
	})

	return filtered, func() {
		filtered = filtered[:0]
		hKvPool.Put(filtered)
	}
}

// filteredAndSortedKeyQueriesInPlace - filters an input slice, be careful!
func (e *Entry) filteredAndSortedKeyQueriesInPlace(queries [][2][]byte) (kvPairs [][2][]byte) {
	filtered := queries[:0]

	allowed := e.rule.CacheKey.QueryBytes
	for _, pair := range queries {
		key := pair[0]
		keep := false
		for _, ak := range allowed {
			if bytes.HasPrefix(key, ak) {
				keep = true
				break
			}
		}
		if keep {
			filtered = append(filtered, pair)
		}
	}

	sort.Slice(filtered, func(i, j int) bool {
		return bytes.Compare(filtered[i][0], filtered[j][0]) < 0
	})

	return filtered
}

// filteredAndSortedKeyHeadersInPlace - filters an input slice, be careful!
func (e *Entry) filteredAndSortedKeyHeadersInPlace(headers [][2][]byte) (kvPairs [][2][]byte) {
	filtered := headers[:0]
	allowedMap := e.rule.CacheKey.HeadersMap

	for _, pair := range headers {
		if _, ok := allowedMap[unsafe.String(unsafe.SliceData(pair[0]), len(pair[0]))]; ok {
			filtered = append(filtered, pair)
		}
	}

	sort.Slice(filtered, func(i, j int) bool {
		return bytes.Compare(filtered[i][0], filtered[j][0]) < 0
	})

	return filtered
}

// SetLruListElement sets the LRU list element pointer.
func (e *Entry) SetLruListElement(el *list.Element[*VersionPointer]) {
	e.lruListElem.Store(el)
}

// LruListElement returns the LRU list element pointer (for LRU cache management).
func (e *Entry) LruListElement() *list.Element[*VersionPointer] {
	return e.lruListElem.Load()
}

// ShouldBeRefreshed implements probabilistic refresh logic ("beta" algorithm).
// Returns true if the entry is stale and, with a probability proportional to its staleness, should be refreshed now.
func (e *Entry) ShouldBeRefreshed(cfg *config.Cache) bool {
	if e == nil {
		return false
	}

	now := time.Now().UnixNano()
	remaining := atomic.LoadInt64(&e.willUpdateAt) - now

	if remaining < 0 {
		remaining = 0 // min clamp is zero, using only probability refresh, forced refresh does not used at all!
		// the line above means that you must not return true value here!
	}

	interval := e.rule.TTL.Nanoseconds()
	if interval == 0 {
		interval = cfg.Cache.Refresh.TTL.Nanoseconds()
	}

	beta := e.rule.Beta
	if beta == 0 {
		beta = cfg.Cache.Refresh.Beta
	}

	x := 1.0 - float64(remaining)/float64(interval)
	if x < 0 {
		x = 0
	} else if x > 1 {
		x = 1
	}

	prob := 1 - math.Exp(-beta*x)
	return rand.Float64() < prob
}

// Revalidate calls the revalidator closure to fetch fresh data and updates the timestamp.
func (e *Entry) Revalidate() error {
	path, query, headers, responseHeaders, _, _, err := e.Payload()
	defer e.ReleasePayload(headers, responseHeaders)
	if err != nil {
		return err
	}

	statusCode, respHeaders, body, releaser, err := e.revalidator(e.rule, path, query, headers)
	defer releaser()
	if err != nil {
		return err
	}
	e.SetPayload(path, query, headers, respHeaders, body, statusCode)

	if statusCode == http.StatusOK {
		atomic.StoreInt64(&e.willUpdateAt, time.Now().UnixNano()+e.rule.TTL.Nanoseconds())
	} else {
		atomic.StoreInt64(&e.willUpdateAt, time.Now().UnixNano()+e.rule.ErrorTTL.Nanoseconds())
	}

	return nil
}

func (e *Entry) ToBytes() (data []byte, releaseFn func()) {
	var scratch8 [8]byte
	var scratch4 [4]byte

	payload := e.PayloadBytes()
	rulePath := e.Rule().PathBytes

	// Забираем buffer из пула и очищаем
	buf := bufPool.Get().(*bytes.Buffer)
	releaseFn = func() {
		buf.Reset()
		bufPool.Put(buf)
	}

	// === RulePath ===
	binary.LittleEndian.PutUint32(scratch4[:], uint32(len(rulePath)))
	buf.Write(scratch4[:])
	buf.Write(rulePath)

	// === Key ===
	binary.LittleEndian.PutUint64(scratch8[:], e.key)
	buf.Write(scratch8[:])

	// === Shard ===
	binary.LittleEndian.PutUint64(scratch8[:], e.shard)
	buf.Write(scratch8[:])

	// === Fingerprint ===
	buf.Write(e.fingerprint[:])

	// === IsCompressed ===
	if e.IsCompressed() {
		buf.WriteByte(1)
	} else {
		buf.WriteByte(0)
	}

	// === WillUpdateAt ===
	binary.LittleEndian.PutUint64(scratch8[:], uint64(e.WillUpdateAt()))
	buf.Write(scratch8[:])

	// === Payload ===
	binary.LittleEndian.PutUint32(scratch4[:], uint32(len(payload)))
	buf.Write(scratch4[:])
	buf.Write(payload)

	// Возвращаем готовый []byte и release
	return buf.Bytes(), releaseFn
}

func EntryFromBytes(data []byte, cfg *config.Cache, backend repository.Backender) (*Entry, error) {
	var offset int

	// RulePath
	rulePathLen := binary.LittleEndian.Uint32(data[offset:])
	offset += 4
	rulePath := data[offset : offset+int(rulePathLen)]
	offset += int(rulePathLen)

	rule := MatchRule(cfg, rulePath)
	if rule == nil {
		return nil, fmt.Errorf("rule not found for path: '%s'", string(rulePath))
	}

	// Key
	key := binary.LittleEndian.Uint64(data[offset:])
	offset += 8

	// Shard
	shard := binary.LittleEndian.Uint64(data[offset:])
	offset += 8

	// Fingerprint
	var fp [16]byte
	copy(fp[:], data[offset:offset+16])
	offset += 16

	// IsCompressed
	compressed := data[offset] == 1
	var isCompressed int64
	if compressed {
		isCompressed = 1
	}
	offset += 1

	// WillUpdateAt
	willUpdateAt := int64(binary.LittleEndian.Uint64(data[offset:]))
	offset += 8

	// Payload
	payloadLen := binary.LittleEndian.Uint32(data[offset:])
	offset += 4
	payload := data[offset : offset+int(payloadLen)]

	return NewEntryFromField(
		key, shard, fp, payload, rule,
		backend.RevalidatorMaker(), isCompressed, willUpdateAt,
	), nil
}

func MatchRule(cfg *config.Cache, path []byte) *config.Rule {
	if rule, ok := cfg.Cache.Rules[unsafe.String(unsafe.SliceData(path), len(path))]; ok {
		return rule
	}
	return nil
}

func MatchRuleStr(cfg *config.Cache, path string) *config.Rule {
	if rule, ok := cfg.Cache.Rules[path]; ok {
		return rule
	}
	return nil
}

// SetMapKey is really dangerous - must be used exclusively in tests.
func (e *Entry) SetMapKey(key uint64) *Entry {
	e.key = key
	return e
}

func (e *Entry) DumpPayload() {
	path, query, queryHeaders, responseHeaders, body, status, err := e.Payload()
	defer e.ReleasePayload(queryHeaders, responseHeaders)
	if err != nil {
		log.Error().Err(err).Msg("[dump] failed to unpack payload")
		return
	}

	fmt.Printf("\n========== DUMP PAYLOAD ==========\n")
	fmt.Printf("Key:          %d\n", e.key)
	fmt.Printf("Shard:        %d\n", e.shard)
	fmt.Printf("IsCompressed: %v\n", e.IsCompressed())
	fmt.Printf("WillUpdateAt: %s\n", time.Unix(0, e.WillUpdateAt()).Format(time.RFC3339Nano))
	fmt.Printf("----------------------------------\n")

	fmt.Printf("Path:   %q\n", string(path))
	fmt.Printf("Query:  %q\n", string(query))
	fmt.Printf("StatusCode: %d\n", status)

	fmt.Printf("\nQuery Headers:\n")
	if len(queryHeaders) == 0 {
		fmt.Println("  (none)")
	} else {
		for i, kv := range queryHeaders {
			fmt.Printf("  [%02d] %q : %q\n", i, kv[0], kv[1])
		}
	}

	fmt.Printf("\nResponse Headers:\n")
	if len(responseHeaders) == 0 {
		fmt.Println("  (none)")
	} else {
		for i, kv := range responseHeaders {
			fmt.Printf("  [%02d] %q : %q\n", i, kv[0], kv[1])
		}
	}

	fmt.Printf("\nBody (%d bytes):\n", len(body))
	if len(body) > 0 {
		const maxLen = 500
		if len(body) > maxLen {
			fmt.Printf("  %q ... [truncated, total %d bytes]\n", body[:maxLen], len(body))
		} else {
			fmt.Printf("  %q\n", body)
		}
	}

	fmt.Println("==================================")
}
