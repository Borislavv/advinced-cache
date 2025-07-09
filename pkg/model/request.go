package model

import (
	"errors"
	"fmt"
	"github.com/Borislavv/advanced-cache/pkg/config"
	"net/http"
	"strings"
	"unsafe"
)

type Request struct {
	rule    *config.Rule // possibly nil pointer (be careful)
	key     uint64
	shard   uint64
	path    []byte
	query   [][2][]byte
	headers [][2][]byte
}

func NewRequestFromNetHttp(cfg *config.Cache, r *http.Request) (*Request, error) {
	//internedPath := PathInterner.InternStr(r.URL.Path)
	//
	//// path must be a readonly slice, don't change it anywhere
	//req := &Request{path: internedPath} // static value (strings are immutable, so easily refer to it)
	//
	//rule := matchRule(cfg, req.path)
	//if rule == nil {
	//	return nil, RuleNotFoundError
	//}
	//req.rule = rule
	//
	//queries := getFilteredAndSortedKeyQueriesNetHttp(r, rule.CacheKey.QueryBytes)
	//headers := getFilteredAndSortedKeyHeadersNetHttp(r, rule.CacheKey.HeadersBytes)
	//
	//req.setUpKeys(queries, headers)
	//
	//return req, nil
	return nil, errors.New("not implemented yet")
}

func NewRequest(rule *config.Rule, key, shard uint64, path []byte, query, headers [][2][]byte) *Request {
	return &Request{key: key, shard: shard, rule: rule, path: path, query: query, headers: headers}
}

func NewRawRequest(rule *config.Rule, key, shard uint64, path []byte, query, headers [][2][]byte) *Request {
	return &Request{key: key, shard: shard, query: query, path: path, headers: headers, rule: rule}
}

func (r *Request) Rule() *config.Rule {
	return r.rule
}

func (r *Request) ToQuery() []byte {
	l := 1
	for _, kv := range r.query {
		l += len(kv[0]) + len(kv[1]) + 1
	}
	buf := make([]byte, 0, l)
	buf = append(buf, []byte("?")...)
	for _, kv := range r.query {
		buf = append(buf, kv[0]...)
		buf = append(buf, []byte("&")...)
		buf = append(buf, kv[1]...)
	}
	return buf
}

func (r *Request) Query() [][2][]byte {
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

//func getFilteredAndSortedKeyQueriesNetHttp(r *http.Request, allowed [][]byte) (kvPairs [][2][]byte) {
//	var filtered = make([][2][]byte, 0, len(allowed))
//	if len(allowed) == 0 {
//		return filtered
//	}
//	for _, pair := range strings.Split(r.URL.RawQuery, "&") {
//		if eqIndex := strings.Index(pair, "="); eqIndex != -1 {
//			key := pair[:eqIndex]
//			keyBytes := unsafe.Slice(unsafe.StringData(key), len(key))
//			value := pair[eqIndex+1:]
//			valueBytes := unsafe.Slice(unsafe.StringData(value), len(value))
//			for _, allowedKey := range allowed {
//				if bytes.HasPrefix(keyBytes, allowedKey) {
//					internedKey := HeaderKeyInterner.Intern(keyBytes)
//					filtered = append(filtered, [2][]byte{internedKey, valueBytes})
//					break
//				}
//			}
//		}
//	}
//	sort.Slice(filtered, func(i, j int) bool {
//		return bytes.Compare(filtered[i][0], filtered[j][0]) < 0
//	})
//	return filtered
//}
//
//func getFilteredAndSortedKeyHeadersNetHttp(r *http.Request, allowed [][]byte) (kvPairs [][2][]byte) {
//	var filtered = make([][2][]byte, 0, len(allowed))
//	if len(allowed) == 0 {
//		return filtered
//	}
//	for key, values := range r.Header {
//		for _, value := range values {
//			keyBytes := unsafe.Slice(unsafe.StringData(key), len(key))
//			valueBytes := unsafe.Slice(unsafe.StringData(value), len(value))
//			for _, allowedKey := range allowed {
//				if bytes.EqualFold(keyBytes, allowedKey) {
//					internedKey := HeaderKeyInterner.Intern(keyBytes)
//					filtered = append(filtered, [2][]byte{internedKey, valueBytes})
//					break
//				}
//			}
//		}
//	}
//	sort.Slice(filtered, func(i, j int) bool {
//		return bytes.Compare(filtered[i][0], filtered[j][0]) < 0
//	})
//	return filtered
//}

//func getFilteredAndSortedKeyQueriesManual(inputKvPairs [][2][]byte, allowed [][]byte) (kvPairs [][2][]byte) {
//	var filtered = make([][2][]byte, 0, len(allowed))
//	if len(allowed) == 0 {
//		return filtered
//	}
//	for _, kvPair := range inputKvPairs {
//		for _, ak := range allowed {
//			if bytes.HasPrefix(kvPair[0], ak) {
//				internedKey := QueryKeyInterner.Intern(kvPair[0])
//				filtered = append(filtered, [2][]byte{internedKey, append([]byte(nil), kvPair[1]...)})
//				break
//			}
//		}
//	}
//	sort.Slice(filtered, func(i, j int) bool {
//		return bytes.Compare(filtered[i][0], filtered[j][0]) < 0
//	})
//	return filtered
//}
//
//func getFilteredAndSortedKeyHeadersManual(inputKvPairs [][2][]byte, allowed [][]byte) (kvPairs [][2][]byte) {
//	var filtered = make([][2][]byte, 0, len(allowed))
//	if len(allowed) == 0 {
//		return filtered
//	}
//	for _, kvPair := range inputKvPairs {
//		for _, allowedKey := range allowed {
//			if bytes.EqualFold(kvPair[0], allowedKey) {
//				internedKey := HeaderKeyInterner.Intern(kvPair[0])
//				filtered = append(filtered, [2][]byte{internedKey, append([]byte(nil), kvPair[1]...)})
//				break
//			}
//		}
//	}
//	sort.Slice(filtered, func(i, j int) bool {
//		return bytes.Compare(filtered[i][0], filtered[j][0]) < 0
//	})
//	return filtered
//}

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
