package model

import (
	"bytes"
	"errors"
	"fmt"
	"github.com/Borislavv/advanced-cache/pkg/config"
	sharded "github.com/Borislavv/advanced-cache/pkg/storage/map"
	"github.com/valyala/fasthttp"
	"github.com/zeebo/xxh3"
	"net/http"
	"sort"
	"strings"
	"sync"
	"unsafe"
)

var RuleNotFoundError = errors.New("rule not found")
var hasherPool = &sync.Pool{New: func() any { return xxh3.New() }}

type Request struct {
	rule    *config.Rule
	key     uint64
	shard   uint64
	path    []byte
	query   [][2][]byte
	headers [][2][]byte
}

func NewRequestFromNetHttp(cfg *config.Cache, r *http.Request) (*Request, error) {
	req := &Request{ // path must be a readonly slice, don't change it anywhere
		path: unsafe.Slice(unsafe.StringData(r.URL.Path), len(r.URL.Path)),
	} // static value (strings are immutable, so easily refer to it)

	req.rule = matchRule(cfg, req.path)
	if req.rule == nil {
		return nil, RuleNotFoundError
	}

	req.query = make([][2][]byte, 0, len(r.URL.Query()))
	for key, values := range r.URL.Query() {
		for _, value := range values {
			req.query = append(req.query, [2][]byte{
				unsafe.Slice(unsafe.StringData(key), len(key)),
				unsafe.Slice(unsafe.StringData(value), len(value)),
			})
		}
	}

	req.headers = make([][2][]byte, 0, len(r.Header))
	for header, v := range r.Header {
		for _, value := range v {
			req.headers = append(req.headers, [2][]byte{
				unsafe.Slice(unsafe.StringData(header), len(header)),
				unsafe.Slice(unsafe.StringData(value), len(value)),
			})
		}
	}

	return req.setUpKeys(), nil
}

func NewRequestFromFasthttp(cfg *config.Cache, r *fasthttp.RequestCtx) (*Request, error) {
	// full separated slice bytes of path a safe for changes due to it copy
	// path in the fasthttp are reusable resource, so just copy it
	req := &Request{path: append([]byte(nil), r.Path()...)}

	req.rule = matchRule(cfg, req.path)
	if req.rule == nil {
		return nil, RuleNotFoundError
	}

	req.query = make([][2][]byte, 0, r.QueryArgs().Len())
	r.QueryArgs().All()(func(arg []byte, value []byte) bool {
		req.query = append(req.query, [2][]byte{
			append([]byte(nil), arg...),
			append([]byte(nil), value...),
		})
		return true
	})

	req.headers = make([][2][]byte, 0, r.Request.Header.Len())
	r.Request.Header.All()(func(header []byte, value []byte) bool {
		req.headers = append(req.headers, [2][]byte{
			append([]byte(nil), header...),
			append([]byte(nil), value...),
		})
		return true
	})

	return req.setUpKeys(), nil
}

func NewRawRequest(cfg *config.Cache, key, shard uint64, path []byte, query, headers [][2][]byte) *Request {
	return &Request{key: key, shard: shard, query: query, path: path, headers: headers, rule: matchRule(cfg, path)}
}

func NewRequest(cfg *config.Cache, path []byte, query [][2][]byte, headers [][2][]byte) *Request {
	return (&Request{path: path, rule: matchRule(cfg, path), query: query, headers: headers}).setUpKeys()
}

func (r *Request) Rule() *config.Rule {
	return r.rule
}

func (r *Request) Query() []byte {
	length := 1
	for _, kv := range r.query {
		length += len(kv[0]) + len(kv[1]) + 2
	}

	buff := make([]byte, 0, length)
	buff = append(buff, []byte("?")...)
	for _, kv := range r.query {
		buff = append(buff, kv[0]...)
		buff = append(buff, []byte("=")...)
		buff = append(buff, kv[1]...)
		buff = append(buff, []byte("&")...)
	}
	if len(buff) > 1 {
		buff = buff[:len(buff)-1] // remove the last & char
	} else {
		buff = buff[:0] // no parameters
	}

	return buff
}

func (r *Request) QueryKV() [][2][]byte {
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

func (r *Request) setUpKeys() *Request {
	var args = make([][2][]byte, 0, len(r.rule.CacheKey.QueryBytes))
	for _, kv := range r.query {
		for _, ak := range r.rule.CacheKey.QueryBytes {
			if bytes.HasPrefix(kv[0], ak) {
				args = append(args, [2][]byte{kv[0], kv[1]})
			}
		}
	}
	sort.Slice(args, func(i, j int) bool {
		return bytes.Compare(args[i][0], args[j][0]) < 0
	})

	var headers = make([][2][]byte, 0, len(r.rule.CacheKey.HeadersBytes))
	for _, kv := range r.headers {
		for _, allowedKey := range r.rule.CacheKey.HeadersBytes {
			if bytes.EqualFold(kv[0], allowedKey) {
				headers = append(headers, [2][]byte{kv[0], kv[1]})
				break
			}
		}
	}
	sort.Slice(headers, func(i, j int) bool {
		return bytes.Compare(headers[i][0], headers[j][0]) < 0
	})

	length := 0
	for _, kv := range args {
		length += len(kv[0]) + len(kv[1])
	}
	for _, kv := range headers {
		length += len(kv[0]) + len(kv[1])
	}

	buf := make([]byte, 0, length)
	for _, kv := range args {
		buf = append(buf, kv[0]...)
		buf = append(buf, kv[1]...)
	}
	for _, kv := range headers {
		buf = append(buf, kv[0]...)
		buf = append(buf, kv[1]...)
	}

	r.key = hash(buf[:])
	r.shard = sharded.MapShardKey(r.key)

	return r
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

func matchRule(cfg *config.Cache, path []byte) *config.Rule {
	for _, rule := range cfg.Cache.Rules {
		if bytes.EqualFold(path, rule.PathBytes) {
			return rule
		}
	}
	return nil
}

func getFilteredAndSortedKeyQueriesNetHttp(args [][2][]byte, allowed [][]byte) (kvPairs [][2][]byte) {
	var filtered = make([][2][]byte, 0, len(allowed))
	if len(allowed) == 0 {
		return filtered
	}
	for _, kv := range args {
		for _, allowedKey := range allowed {
			if bytes.HasPrefix(kv[0], allowedKey) {
				filtered = append(filtered, [2][]byte{kv[0], kv[1]})
				break
			}
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		return bytes.Compare(filtered[i][0], filtered[j][0]) < 0
	})
	return filtered
}

func getFilteredAndSortedKeyHeadersNetHttp(headers [][2][]byte, allowed [][]byte) (kvPairs [][2][]byte) {
	var filtered = make([][2][]byte, 0, len(allowed))
	if len(allowed) == 0 {
		return filtered
	}
	for _, kv := range headers {
		for _, allowedKey := range allowed {
			if bytes.EqualFold(kv[0], allowedKey) {
				filtered = append(filtered, [2][]byte{kv[0], kv[1]})
				break
			}
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		return bytes.Compare(filtered[i][0], filtered[j][0]) < 0
	})
	return filtered
}

func getFilteredAndSortedKeyQueriesFastHttp(args [][2][]byte, allowed [][]byte) (kvPairs [][2][]byte) {
	var filtered = make([][2][]byte, 0, len(allowed))
	if len(allowed) == 0 {
		return filtered
	}
	for _, kv := range args {
		for _, ak := range allowed {
			if bytes.HasPrefix(kv[0], ak) {
				filtered = append(filtered, [2][]byte{kv[0], kv[1]})
			}
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		return bytes.Compare(filtered[i][0], filtered[j][0]) < 0
	})
	return filtered
}

func getFilteredAndSortedKeyHeadersFastHttp(headers [][2][]byte, allowed [][]byte) (kvPairs [][2][]byte) {
	var filtered = make([][2][]byte, 0, len(allowed))
	if len(allowed) == 0 {
		return filtered
	}
	for _, kv := range headers {
		for _, allowedKey := range allowed {
			if bytes.EqualFold(kv[0], allowedKey) {
				filtered = append(filtered, [2][]byte{kv[0], kv[1]})
				break
			}
		}
	}
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
				filtered = append(filtered, [2][]byte{
					append([]byte(nil), kvPair[0]...),
					append([]byte(nil), kvPair[1]...),
				})
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
				filtered = append(filtered, [2][]byte{
					append([]byte(nil), kvPair[0]...),
					append([]byte(nil), kvPair[1]...),
				})
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
		string(r.Query()),
		strings.Join(reqHeaders, "\n\t\t"),
	)
}
