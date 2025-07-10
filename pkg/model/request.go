package model

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/Borislavv/advanced-cache/pkg/config"
	"github.com/Borislavv/advanced-cache/pkg/intern"
	sharded "github.com/Borislavv/advanced-cache/pkg/storage/map"
	"github.com/valyala/bytebufferpool"
	"github.com/valyala/fasthttp"
	"github.com/zeebo/xxh3"
	"net/http"
	"sort"
	"strings"
	"sync"
	"unsafe"
)

var (
	hasherPool        = &sync.Pool{New: func() any { return xxh3.New() }}
	RuleNotFoundError = errors.New("rule not found")
	PathInterner      = intern.NewInterner(64)
	HeaderKeyInterner = intern.NewInterner(64)
	QueryKeyInterner  = intern.NewInterner(256)
)

type Request struct {
	rule    *config.Rule // possibly nil pointer (be careful)
	key     uint64
	shard   uint64
	query   []byte
	path    []byte
	headers [][2][]byte
}

func NewRequestFromNetHttp(cfg *config.Cache, r *http.Request) (*Request, error) {
	internedPath := PathInterner.InternStr(r.URL.Path)

	// path must be a readonly slice, don't change it anywhere
	req := &Request{path: internedPath} // static value (strings are immutable, so easily refer to it)

	rule := MatchRule(cfg, req.path)
	if rule == nil {
		return nil, RuleNotFoundError
	}
	req.rule = rule

	queries := getFilteredAndSortedKeyQueriesNetHttp(r, rule.CacheKey.QueryBytes)
	headers := getFilteredAndSortedKeyHeadersNetHttp(r, rule.CacheKey.HeadersBytes)

	req.setUpManually(queries, headers)

	return req, nil
}

func NewRequestFromFasthttp(cfg *config.Cache, r *fasthttp.RequestCtx) (*Request, error) {
	internedPath := PathInterner.Intern(r.Path())

	req := &Request{path: internedPath}

	req.rule = MatchRule(cfg, internedPath)
	if req.rule == nil {
		return nil, RuleNotFoundError
	}

	queries := getFilteredAndSortedKeyQueriesFastHttp(r, req.rule.CacheKey.QueryBytes)
	headers := getFilteredAndSortedKeyHeadersFastHttp(&r.Request.Header, req.rule.CacheKey.HeadersBytes)

	req.setUpManually(queries, headers)

	return req, nil
}

func NewRawRequest(cfg *config.Cache, key, shard uint64, query, path []byte, headers [][2][]byte) *Request {
	internedPath := PathInterner.Intern(path)
	rule := MatchRule(cfg, internedPath)
	if rule == nil {
		panic("rile is nil for path: " + string(internedPath))
	}
	return &Request{key: key, shard: shard, query: query, path: internedPath, headers: headers, rule: rule}
}

func NewRequest(cfg *config.Cache, path []byte, argsKvPairs [][2][]byte, headersKvPairs [][2][]byte) *Request {
	internedPath := PathInterner.Intern(path)
	req := &Request{path: internedPath, rule: MatchRule(cfg, internedPath)}
	req.setUpManually(
		getFilteredAndSortedKeyQueriesManual(argsKvPairs, req.rule.CacheKey.QueryBytes),
		getFilteredAndSortedKeyHeadersManual(headersKvPairs, req.rule.CacheKey.HeadersBytes),
	)
	return req
}

func (r *Request) Rule() *config.Rule {
	return r.rule
}

func (r *Request) ToQuery() []byte {
	return r.query
}

func (r *Request) Headers() [][2][]byte {
	return r.headers
}

func (r *Request) Path() []byte {
	return r.path
}

func (r *Request) MapKey() uint64 {
	return r.key
}

func (r *Request) ShardKey() uint64 {
	return r.shard
}

func (r *Request) Weight() int64 {
	weight := int64(unsafe.Sizeof(*r)) + int64(len(r.query)) + int64(len(r.path))
	for _, kv := range r.Headers() {
		weight += int64(unsafe.Sizeof(kv)) + int64(len(kv[0])) + int64(len(kv[1]))
	}
	return weight
}

var bufPool = &sync.Pool{
	New: func() interface{} {
		return new(bytes.Buffer)
	},
}

func (r *Request) setUpManually(argsKvPairs [][2][]byte, headersKvPairs [][2][]byte) {
	r.headers = headersKvPairs

	argsLength := 1
	for _, pair := range argsKvPairs {
		argsLength += len(pair[0]) + len(pair[1]) + 2
	}
	headersLength := 0
	for _, pair := range headersKvPairs {
		headersLength += len(pair[0]) + len(pair[1])
	}
	buf := bufPool.Get().(*bytes.Buffer)
	defer func() {
		buf.Reset()
		bufPool.Put(buf)
	}()
	buf.Grow(argsLength + headersLength)

	queryBuf := make([]byte, 0, argsLength)
	queryBuf = append(queryBuf, []byte("?")...)
	for _, pair := range argsKvPairs {
		queryBuf = append(queryBuf, pair[0]...)
		queryBuf = append(queryBuf, []byte("=")...)
		queryBuf = append(queryBuf, pair[1]...)
		queryBuf = append(queryBuf, []byte("&")...)
	}
	if len(queryBuf) > 1 {
		queryBuf = queryBuf[:len(queryBuf)-1] // remove the last & char
	} else {
		queryBuf = queryBuf[:0] // no parameters
	}
	buf.Write(queryBuf)

	for _, pair := range headersKvPairs {
		buf.Write(pair[0])
		buf.Write(pair[1])
	}

	//r.key = hash(buf)
	r.query = queryBuf[:]
	r.shard = sharded.MapShardKey(r.key)
}

func hash(buf *bytebufferpool.ByteBuffer) uint64 {
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

func MatchRule(cfg *config.Cache, path []byte) *config.Rule {
	for _, rule := range cfg.Cache.Rules {
		if bytes.EqualFold(path, rule.PathBytes) {
			return rule
		}
	}
	return nil
}

func getFilteredAndSortedKeyQueriesNetHttp(r *http.Request, allowed [][]byte) (kvPairs [][2][]byte) {
	var filtered = make([][2][]byte, 0, len(allowed))
	if len(allowed) == 0 {
		return filtered
	}
	for _, pair := range strings.Split(r.URL.RawQuery, "&") {
		if eqIndex := strings.Index(pair, "="); eqIndex != -1 {
			key := pair[:eqIndex]
			keyBytes := unsafe.Slice(unsafe.StringData(key), len(key))
			value := pair[eqIndex+1:]
			valueBytes := unsafe.Slice(unsafe.StringData(value), len(value))
			for _, allowedKey := range allowed {
				if bytes.HasPrefix(keyBytes, allowedKey) {
					internedKey := HeaderKeyInterner.Intern(keyBytes)
					filtered = append(filtered, [2][]byte{internedKey, valueBytes})
					break
				}
			}
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		return bytes.Compare(filtered[i][0], filtered[j][0]) < 0
	})
	return filtered
}

func getFilteredAndSortedKeyHeadersNetHttp(r *http.Request, allowed [][]byte) (kvPairs [][2][]byte) {
	var filtered = make([][2][]byte, 0, len(allowed))
	if len(allowed) == 0 {
		return filtered
	}
	for key, values := range r.Header {
		for _, value := range values {
			keyBytes := unsafe.Slice(unsafe.StringData(key), len(key))
			valueBytes := unsafe.Slice(unsafe.StringData(value), len(value))
			for _, allowedKey := range allowed {
				if bytes.EqualFold(keyBytes, allowedKey) {
					internedKey := HeaderKeyInterner.Intern(keyBytes)
					filtered = append(filtered, [2][]byte{internedKey, valueBytes})
					break
				}
			}
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		return bytes.Compare(filtered[i][0], filtered[j][0]) < 0
	})
	return filtered
}

func getFilteredAndSortedKeyQueriesFastHttp(ctx *fasthttp.RequestCtx, allowed [][]byte) (kvPairs [][2][]byte) {
	var filtered = make([][2][]byte, 0, len(allowed))
	if len(allowed) == 0 {
		return filtered
	}
	ctx.QueryArgs().VisitAll(func(k, v []byte) {
		for _, ak := range allowed {
			if bytes.HasPrefix(k, ak) {
				internedKey := QueryKeyInterner.Intern(k)
				filtered = append(filtered, [2][]byte{internedKey, append([]byte(nil), v...)})
				break
			}
		}
	})
	sort.Slice(filtered, func(i, j int) bool {
		return bytes.Compare(filtered[i][0], filtered[j][0]) < 0
	})
	return filtered
}

func getFilteredAndSortedKeyHeadersFastHttp(r *fasthttp.RequestHeader, allowed [][]byte) (kvPairs [][2][]byte) {
	var filtered = make([][2][]byte, 0, len(allowed))
	if len(allowed) == 0 {
		return filtered
	}
	r.VisitAll(func(key, value []byte) {
		for _, allowedKey := range allowed {
			if bytes.EqualFold(key, allowedKey) {
				internedKey := HeaderKeyInterner.Intern(key)
				filtered = append(filtered, [2][]byte{internedKey, append([]byte(nil), value...)})
				break
			}
		}
	})
	sort.Slice(filtered, func(i, j int) bool {
		return bytes.Compare(filtered[i][0], filtered[j][0]) < 0
	})
	return filtered
}

func getFilteredAndSortedKeyQueriesManual(inputKvPairs [][2][]byte, allowed [][]byte) (kvPairs [][2][]byte) {
	var filtered = make([][2][]byte, 0, len(allowed))
	if len(allowed) == 0 {
		return filtered
	}
	for _, kvPair := range inputKvPairs {
		for _, ak := range allowed {
			if bytes.HasPrefix(kvPair[0], ak) {
				internedKey := QueryKeyInterner.Intern(kvPair[0])
				filtered = append(filtered, [2][]byte{internedKey, append([]byte(nil), kvPair[1]...)})
				break
			}
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		return bytes.Compare(filtered[i][0], filtered[j][0]) < 0
	})
	return filtered
}

func getFilteredAndSortedKeyHeadersManual(inputKvPairs [][2][]byte, allowed [][]byte) (kvPairs [][2][]byte) {
	var filtered = make([][2][]byte, 0, len(allowed))
	if len(allowed) == 0 {
		return filtered
	}
	for _, kvPair := range inputKvPairs {
		for _, allowedKey := range allowed {
			if bytes.EqualFold(kvPair[0], allowedKey) {
				internedKey := HeaderKeyInterner.Intern(kvPair[0])
				filtered = append(filtered, [2][]byte{internedKey, append([]byte(nil), kvPair[1]...)})
				break
			}
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		return bytes.Compare(filtered[i][0], filtered[j][0]) < 0
	})
	return filtered
}

func (r *Request) PrintDump(marker string) {
	reqHeaders := make([]string, 0, len(r.Headers()))
	for _, header := range r.Headers() {
		reqHeaders = append(reqHeaders, fmt.Sprintf("%s: %s", header[0], header[1]))
	}

	fmt.Printf(
		"[DUMP-%v] Request {\n"+
			"\t\tMapKey:   %d\n"+
			"\t\tShardKey: %d\n"+
			"\t\tQuery:    %s\n"+
			"\t\tHeaders:  %s\n"+
			"}\n",
		marker,
		r.MapKey(),
		r.ShardKey(),
		string(r.ToQuery()),
		strings.Join(reqHeaders, "\n\t\t"),
	)
}
