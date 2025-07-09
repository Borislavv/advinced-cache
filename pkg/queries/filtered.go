package queries

import (
	"bytes"
	"github.com/Borislavv/advanced-cache/pkg/intern"
	"github.com/valyala/fasthttp"
	"sort"
)

func FilteredAndSortedKeyQueriesFastHttp(ctx *fasthttp.RequestCtx, allowed [][]byte) [][2][]byte {
	if len(allowed) == 0 {
		return nil
	}

	var filtered = make([][2][]byte, 0, len(allowed))

	ctx.QueryArgs().All()(func(k, v []byte) bool {
		for _, ak := range allowed {
			if bytes.HasPrefix(k, ak) {
				internedKey := intern.QueryKeyInterner.Intern(k, true)
				// NOTE: safe copy for value
				filtered = append(filtered, [2][]byte{internedKey, append([]byte(nil), v...)})
				break
			}
		}
		return true
	})

	// Sort in place
	sort.Slice(filtered, func(i, j int) bool {
		return bytes.Compare(filtered[i][0], filtered[j][0]) < 0
	})

	return filtered
}

func FilteredAndSortedKeyQueries(values [][2][]byte, allowed [][]byte) [][2][]byte {
	if len(allowed) == 0 {
		return nil
	}

	var filtered = make([][2][]byte, 0, len(allowed))

	for _, kv := range values {
		for _, ak := range allowed {
			if bytes.HasPrefix(kv[0], ak) {
				internedKey := intern.QueryKeyInterner.Intern(kv[0], false)
				// NOTE: safe copy for value
				filtered = append(filtered, [2][]byte{internedKey, append([]byte(nil), kv[1]...)})
				break
			}
		}
	}

	// Sort in place
	sort.Slice(filtered, func(i, j int) bool {
		return bytes.Compare(filtered[i][0], filtered[j][0]) < 0
	})

	return filtered
}
