package model

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"github.com/Borislavv/advanced-cache/pkg/config"
	"github.com/Borislavv/advanced-cache/pkg/list"
	"github.com/Borislavv/advanced-cache/pkg/pools"
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

var EntriesPool = sync.Pool{
	New: func() interface{} {
		return new(Entry).Init()
	},
}

type Revalidator func(
	path []byte, query []byte, queryHeaders [][2][]byte,
) (
	status int, headers [][2][]byte, body []byte, releaseFn func(), err error,
)

// Entry is the packed request+response payload
type Entry struct {
	key          uint64
	shard        uint64
	rule         *config.Rule
	payload      *atomic.Pointer[[]byte]
	lruListElem  *atomic.Pointer[list.Element[*Entry]]
	revalidator  Revalidator
	willUpdateAt int64
	isCompressed int64
}

func (e *Entry) Init() *Entry {
	e.lruListElem = &atomic.Pointer[list.Element[*Entry]]{}
	e.payload = &atomic.Pointer[[]byte]{}
	payload := make([]byte, 0, 8)
	e.payload.Store(&payload)
	return e
}

func NewEntryFromField(
	key uint64,
	shard uint64,
	payload []byte,
	rule *config.Rule,
	revalidator Revalidator,
	isCompressed bool,
	willUpdateAt int64,
) *Entry {
	var isCompressedInt int64
	if isCompressed {
		isCompressedInt = 1
	}
	e := &Entry{
		key:          key,
		shard:        shard,
		rule:         rule,
		payload:      &atomic.Pointer[[]byte]{},
		lruListElem:  &atomic.Pointer[list.Element[*Entry]]{},
		revalidator:  revalidator,
		isCompressed: isCompressedInt,
		willUpdateAt: willUpdateAt,
	}
	e.payload.Store(&payload)
	return e
}

//var (
//	bufPool        = &sync.Pool{New: func() any { return new(bytes.Buffer) }}
//	GzipBufferPool = &sync.Pool{New: func() any { return new(bytes.Buffer) }}
//	GzipWriterPool = &sync.Pool{New: func() any {
//		w, _ := gzip.NewWriterLevel(nil, gzip.BestSpeed)
//		return w
//	}}
//	RuleNotFoundError = errors.New("rule not found")
//)
//
//const gzipThreshold = 1024

func (e *Entry) MapKey() uint64   { return e.key }
func (e *Entry) ShardKey() uint64 { return e.shard }

type Releaser func()

// NewEntry accepts path, query and request headers as bytes slices.
func NewEntry(cfg *config.Cache, r *fasthttp.RequestCtx) (*Entry, Releaser, error) {
	rule := MatchRule(cfg, r.Path())
	if rule == nil {
		return nil, func() {}, RuleNotFoundError
	}

	entry := EntriesPool.Get().(*Entry)
	entry.rule = rule

	filteredQueries, queriesReleaser := entry.getFilteredAndSortedKeyQueries(r)
	defer queriesReleaser()

	filteredHeaders, headersReleaser := entry.getFilteredAndSortedKeyHeaders(r)
	defer headersReleaser()

	return entry.calculateAndSetUpKeys(filteredQueries, filteredHeaders), entry.Release, nil
}

func NewEntryManual(cfg *config.Cache, path, query []byte, headers [][2][]byte, revalidator Revalidator) (*Entry, Releaser, error) {
	rule := MatchRule(cfg, path)
	if rule == nil {
		return nil, func() {}, RuleNotFoundError
	}

	entry := EntriesPool.Get().(*Entry)
	entry.rule = rule
	entry.revalidator = revalidator

	// initial request payload (response payload will be filled later)
	entry.SetPayload(path, query, headers, nil, nil, 0)

	// it's necessary due to have own query buffer inside entry, further below we will be referring to sub slices of query
	_, payloadQuery, payloadHeaders, _, _, _, payloadReleaser, err := entry.Payload()
	defer payloadReleaser()
	if err != nil {
		return nil, entry.Release, err
	}

	queries, queriesReleaser := parseQuery(payloadQuery) // here, we are referring to the same query buffer which used in payload which have been mentioned before
	defer queriesReleaser()                              // this is really reduce memory usage and GC pressure

	filteredQueries := entry.filteredAndSortedKeyQueriesInPlace(queries)
	filteredHeaders := entry.filteredAndSortedKeyHeadersInPlace(payloadHeaders)

	return entry.calculateAndSetUpKeys(filteredQueries, filteredHeaders), entry.Release, nil
}

// Release used as a Releaser and returns buffers back to pools.
func (e *Entry) Release() {
	e.key = 0
	e.shard = 0
	e.rule = nil
	e.willUpdateAt = 0
	e.isCompressed = 0
	e.revalidator = nil
	e.lruListElem.Store(nil)

	payload := e.payload.Load()
	*payload = (*payload)[:0]
	e.payload.Store(payload)

	EntriesPool.Put(e)
}

func (e *Entry) calculateAndSetUpKeys(filteredQueries, filteredHeaders [][2][]byte) *Entry {
	l := 0
	for _, pair := range filteredQueries {
		l += len(pair[0]) + len(pair[1])
	}
	for _, pair := range filteredHeaders {
		l += len(pair[0]) + len(pair[1])
	}
	buf := make([]byte, 0, l)
	for _, pair := range filteredQueries {
		buf = append(buf, pair[0]...)
		buf = append(buf, pair[1]...)
	}
	for _, pair := range filteredHeaders {
		buf = append(buf, pair[0]...)
		buf = append(buf, pair[1]...)
	}

	hasher := hasherPool.Get().(*xxh3.Hasher)
	defer func() {
		hasher.Reset()
		hasherPool.Put(hasher)
	}()
	if _, err := hasher.Write(buf); err != nil {
		panic(err)
	}

	//e.key = xxh3.Hash(buf)
	e.key = hasher.Sum64()
	e.shard = sharded.MapShardKey(e.key)

	return e
}

func (e *Entry) SetRevalidator(revalidator Revalidator) {
	e.revalidator = revalidator
}

// SetPayload packs and gzip-compresses the entire payload: Path, Query, QueryHeaders, Status, ResponseHeaders, Body.
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

	// Status
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

	//if status > 0 {
	//	npath, nquery, _, _, nbody, _, _, err := e.Payload()
	//	if err != nil {
	//		panic(err)
	//	}
	//
	//	fmt.Printf("path=%v, query=%v, body=%v\n", string(npath), string(nquery), string(nbody))
	//}
}

var emptyFn = func() {}

// Payload decompresses the entire payload and unpacks it into fields.
func (e *Entry) Payload() (
	path []byte,
	query []byte,
	queryHeaders [][2][]byte,
	responseHeaders [][2][]byte,
	body []byte,
	status int,
	releaseFn func(),
	err error,
) {
	payloadPtr := e.payload.Load()
	if payloadPtr == nil {
		return nil, nil, nil, nil, nil, 0, emptyFn, fmt.Errorf("payload is nil")
	}

	var rawPayload []byte
	if atomic.LoadInt64(&e.isCompressed) == 1 {
		// TODO need to move it to sync.Pool
		gr, err := gzip.NewReader(bytes.NewReader(*payloadPtr))
		if err != nil {
			return nil, nil, nil, nil, nil, 0, emptyFn, fmt.Errorf("make gzip reader error: %w", err)
		}
		defer gr.Close()

		rawPayload, err = io.ReadAll(gr)
		if err != nil {
			return nil, nil, nil, nil, nil, 0, emptyFn, fmt.Errorf("gzip read failed: %w", err)
		}
	} else {
		rawPayload = *payloadPtr
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

	// --- Status
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
	bodyLen := binary.LittleEndian.Uint32(rawPayload[offset:])
	offset += 4
	if offset+int(bodyLen) > len(rawPayload) {
		fmt.Printf("payload: '%v', part: '%v'\n", string(rawPayload), string(rawPayload[offset:]))
		panic("found")
	}
	body = rawPayload[offset:]

	releaseFn = func() {
		queryHeaders = queryHeaders[:0]
		pools.KeyValueSlicePool.Put(queryHeaders)
		responseHeaders = responseHeaders[:0]
		pools.KeyValueSlicePool.Put(responseHeaders)
	}

	return
}

func (e *Entry) Rule() *config.Rule {
	return e.rule
}

func (e *Entry) PayloadBytes() []byte {
	return *e.payload.Load()
}

func (e *Entry) Weight() int64 {
	return int64(unsafe.Sizeof(*e)) + int64(cap(*e.payload.Load()))
}

func (e *Entry) IsCompressed() bool {
	return atomic.LoadInt64(&e.isCompressed) == 1
}

func (e *Entry) WillUpdateAt() int64 {
	return atomic.LoadInt64(&e.willUpdateAt)
}

func parseQuery(b []byte) (queries [][2][]byte, releaseFn func()) {
	b = bytes.TrimLeft(b, "?")

	queries = pools.KeyValueSlicePool.Get().([][2][]byte)
	queries = queries[:0]

	type state struct {
		kIdx   int
		vIdx   int
		kFound bool
		vFound bool
	}

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

	return queries, func() {
		queries = queries[:0]
		pools.KeyValueSlicePool.Put(queries)
	}
}

func (e *Entry) getFilteredAndSortedKeyQueries(r *fasthttp.RequestCtx) (kvPairs [][2][]byte, releaseFn func()) {
	var filtered = pools.KeyValueSlicePool.Get().([][2][]byte)
	releaseFn = func() {
		filtered = filtered[:0]
		pools.KeyValueSlicePool.Put(filtered)
	}
	if cap(filtered) == 0 {
		return filtered, releaseFn
	}
	r.QueryArgs().All()(func(key, value []byte) bool {
		for _, ak := range e.rule.CacheKey.QueryBytes {
			if bytes.HasPrefix(key, ak) {
				filtered = append(filtered, [2][]byte{key, value})
				break
			}
		}
		return true
	})
	sort.Slice(filtered, func(i, j int) bool {
		return bytes.Compare(filtered[i][0], filtered[j][0]) < 0
	})
	return filtered, releaseFn
}

func (e *Entry) getFilteredAndSortedKeyHeaders(r *fasthttp.RequestCtx) (kvPairs [][2][]byte, releaseFn func()) {
	var filtered = make([][2][]byte, 0, len(e.rule.CacheKey.HeadersBytes))
	releaseFn = func() {
		filtered = filtered[:0]
		pools.KeyValueSlicePool.Put(filtered)
	}
	if cap(filtered) == 0 {
		return filtered, releaseFn
	}
	r.Request.Header.All()(func(key, value []byte) bool {
		for _, allowedKey := range e.rule.CacheKey.HeadersBytes {
			if bytes.EqualFold(key, allowedKey) {
				filtered = append(filtered, [2][]byte{key, value})
				break
			}
		}
		return true
	})
	sort.Slice(filtered, func(i, j int) bool {
		return bytes.Compare(filtered[i][0], filtered[j][0]) < 0
	})
	return filtered, releaseFn
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
	allowed := e.rule.CacheKey.HeadersBytes

	for _, pair := range headers {
		key := pair[0]
		keep := false
		for _, ak := range allowed {
			if bytes.EqualFold(key, ak) {
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

// SetLruListElement sets the LRU list element pointer.
func (e *Entry) SetLruListElement(el *list.Element[*Entry]) {
	e.lruListElem.Store(el)
}

// LruListElement returns the LRU list element pointer (for LRU cache management).
func (e *Entry) LruListElement() *list.Element[*Entry] {
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
		remaining = 0 // it's time already
	}

	interval := e.rule.TTL.Nanoseconds()
	if interval == 0 {
		interval = cfg.Cache.Refresh.TTL.Nanoseconds()
	}

	beta := e.rule.Beta
	if beta == 0 {
		beta = cfg.Cache.Refresh.Beta
	}

	// Probability: higher as we approach ShouldRevalidatedAt
	prob := 1 - math.Exp(-beta*(1.0-(float64(remaining)/float64(interval))))

	return rand.Float64() < prob
}

// Revalidate calls the revalidator closure to fetch fresh data and updates the timestamp.
func (e *Entry) Revalidate() error {
	path, query, headers, _, _, _, release, err := e.Payload()
	defer release()
	if err != nil {
		return err
	}

	statusCode, respHeaders, body, releaser, err := e.revalidator(path, query, headers)
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

func (e *Entry) DumpPayload() {
	path, query, queryHeaders, responseHeaders, body, status, releaseFn, err := e.Payload()
	defer releaseFn()
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
	fmt.Printf("Status: %d\n", status)

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
