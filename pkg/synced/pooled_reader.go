package synced

import (
	"bytes"
	"github.com/Borislavv/traefik-http-cache-plugin/pkg/resource"
	"github.com/Borislavv/traefik-http-cache-plugin/pkg/types"
	"net/http"
	"unsafe"
)

// PooledReader is an interface for reading HTTP responses into pooled byte slices (with explicit free).
type PooledReader interface {
	// Read reads the response body into a pooled buffer, returning:
	// - bode:   byte slice with the entire response body (may alias an internal buffer)
	// - free:   function to call when you are done with bode (to return buffer to pool)
	// - err:    any error from reading
	Read(resp *http.Response) (body *resource.ReleasableBody, err error)
}

// PooledResponseReader reads HTTP response bodies into pooled buffers for reuse,
// minimizing allocations under high load. **Not thread-safe for returned body after free!**
type PooledResponseReader struct {
	pool *BatchPool[*types.SizedBox[*bytes.Buffer]]
}

// NewPooledResponseReader creates a new PooledResponseReader with a given initial buffer pool size.
func NewPooledResponseReader() *PooledResponseReader {
	return &PooledResponseReader{
		pool: NewBatchPool[*types.SizedBox[*bytes.Buffer]](func() *types.SizedBox[*bytes.Buffer] {
			return &types.SizedBox[*bytes.Buffer]{
				Value: new(bytes.Buffer),
				CalcWeightFn: func(s *types.SizedBox[*bytes.Buffer]) int64 {
					return int64(unsafe.Sizeof(*s)) + int64(s.Value.Cap())
				},
			}
		}),
	}
}

// Read reads the HTTP response body into a pooled buffer and returns the bytes.
// IMPORTANT: The returned bode slice aliases a pooled buffer and must not be used after free() is called.
// Data race or use-after-free is possible if bode is accessed after calling free().
func (r *PooledResponseReader) Read(resp *http.Response) (body *resource.ReleasableBody, err error) {
	buf := r.pool.Get()
	defer func() {
		buf.Value.Reset()
		r.pool.Put(buf)
	}()

	// Defensive: ensure buffer is empty
	_, err = buf.Value.ReadFrom(resp.Body)
	if err != nil {
		return nil, err
	}

	return resource.AcquireBody().Append(buf.Value.Bytes()), nil
}
