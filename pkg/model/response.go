package model

import (
	"context"
	"errors"
	"github.com/Borislavv/traefik-http-cache-plugin/pkg/config"
	"github.com/Borislavv/traefik-http-cache-plugin/pkg/storage/list"
	"github.com/Borislavv/traefik-http-cache-plugin/pkg/synced"
	"math"
	"math/rand/v2"
	"net/http"
	"sync/atomic"
	"time"
	"unsafe"
)

const gzipThreshold = 1024 // Minimum body size to apply gzip compression

// -- Internal pools for efficient memory management --

var (
	responsesPool = synced.NewBatchPool[*Response](func() *Response {
		return new(Response).Init()
	})
)

// Response is the main cache object, holding the request, payload, metadata, and list pointers.
type Response struct {
	cfg           *config.Cache                                     // Immutable field
	request       *atomic.Pointer[Request]                          // Associated Key
	data          *atomic.Pointer[Data]                             // Cached data
	lruListElem   *atomic.Pointer[list.Element[*Response]]          // Pointer for LRU list (per-shard)
	revalidator   func(ctx context.Context) (data *Data, err error) // Closure for refresh/revalidation
	weight        int64                                             // Weight in bytes
	refCount      int64                                             // refCount for concurrent/lifecycle management
	isDoomed      int64                                             // "Doomed" flag for objects marked for delete but still referenced
	revalidatedAt int64                                             // Last revalidated time (nanoseconds since epoch)
}

// NewResponse constructs a new Response using memory pools and sets up all fields.
func NewResponse(
	data *Data, req *Request, cfg *config.Cache,
	revalidator func(ctx context.Context) (data *Data, err error),
) (*Response, error) {
	return responsesPool.Get().Init().SetUp(cfg, data, req, revalidator), nil
}

// Init ensures all pointers are non-nil after pool Get.
func (r *Response) Init() *Response {
	if r.request == nil {
		r.request = &atomic.Pointer[Request]{}
	}
	if r.data == nil {
		r.data = &atomic.Pointer[Data]{}
	}
	if r.lruListElem == nil {
		r.lruListElem = &atomic.Pointer[list.Element[*Response]]{}
	}
	return r
}

// SetUp stores the Data, Request, and config-driven fields into the Response.
func (r *Response) SetUp(
	cfg *config.Cache,
	data *Data,
	req *Request,
	revalidator func(ctx context.Context) (data *Data, err error),
) *Response {
	r.cfg = cfg
	r.data.Store(data)
	r.request.Store(req)
	r.revalidator = revalidator
	r.revalidatedAt = time.Now().UnixNano()
	r.weight = r.setUpWeight()
	return r
}

// --- Response API ---

func (r *Response) Touch() *Response {
	atomic.StoreInt64(&r.revalidatedAt, time.Now().UnixNano())
	return r
}

// ToQuery returns the args representation of the request.
func (r *Response) ToQuery() []byte {
	return r.request.Load().ToQuery()
}

// MapKey returns the key of the associated request.
func (r *Response) MapKey() uint64 {
	return r.request.Load().MapKey()
}

// ShardKey returns the shard key of the associated request.
func (r *Response) ShardKey() uint64 {
	return r.request.Load().ShardKey()
}

// ShouldBeRefreshed implements probabilistic refresh logic ("beta" algorithm).
// Returns true if the entry is stale and, with a probability proportional to its staleness, should be refreshed now.
func (r *Response) ShouldBeRefreshed() bool {
	// Don't refresh doomed items.
	if r.IsDoomed() {
		return false
	}

	var (
		beta          = r.cfg.Cache.Refresh.Beta
		interval      = r.cfg.Cache.Refresh.TTL
		minStale      = r.cfg.Cache.Refresh.MinStale
		revalidatedAt = atomic.LoadInt64(&r.revalidatedAt)
	)

	if r.data.Load().statusCode != http.StatusOK {
		interval = interval / 10 // On stale will be used 10% of origin interval.
		minStale = minStale / 10 // On stale will be used 10% of origin stale duration.
	}

	// hard check that min
	if age := time.Since(time.Unix(0, revalidatedAt)).Nanoseconds(); age > minStale.Nanoseconds() {
		return rand.Float64() >= math.Exp((-beta)*float64(age)/float64(interval))
	}

	return false
}

// Revalidate calls the revalidator closure to fetch fresh data and updates the timestamp.
func (r *Response) Revalidate(ctx context.Context) error {
	data, err := r.revalidator(ctx)
	if err != nil {
		return err
	}

	r.data.Store(data)
	atomic.AddInt64(&r.weight, data.Weight()-r.data.Load().Weight())
	atomic.StoreInt64(&r.revalidatedAt, time.Now().UnixNano())

	return nil
}

// Request returns the request pointer.
func (r *Response) Request() *Request {
	return r.request.Load()
}

// LruListElement returns the LRU list element pointer (for LRU cache management).
func (r *Response) LruListElement() *list.Element[*Response] {
	return r.lruListElem.Load()
}

// SetLruListElement sets the LRU list element pointer.
func (r *Response) SetLruListElement(el *list.Element[*Response]) {
	r.lruListElem.Store(el)
}

// Data returns the underlying Data payload.
func (r *Response) Data() *Data {
	return r.data.Load()
}

// Body returns the response body.
func (r *Response) Body() []byte {
	return r.data.Load().Body()
}

// Headers returns the HTTP h.
func (r *Response) Headers() http.Header {
	return r.data.Load().Headers()
}

// RevalidatedAt returns the last revalidation time (as time.Time).
func (r *Response) RevalidatedAt() time.Time {
	return time.Unix(0, atomic.LoadInt64(&r.revalidatedAt))
}

func (r *Response) setUpWeight() int64 {
	var size = int(unsafe.Sizeof(*r))

	data := r.data.Load()
	if data != nil {
		for key, values := range data.headers {
			size += len(key)
			for _, val := range values {
				size += len(val)
			}
		}
		size += int(data.body.Weight())
	}

	return int64(size) + r.Request().Weight()
}

// Weight estimates the in-memory size of this response (including dynamic fields).
func (r *Response) Weight() int64 {
	return r.weight
}

// IsDoomed returns true if this object is scheduled for deletion.
func (r *Response) IsDoomed() bool {
	return atomic.LoadInt64(&r.isDoomed) == 1
}

// MarkAsDoomed marks the response as scheduled for delete, only if not already doomed.
func (r *Response) MarkAsDoomed() bool {
	return atomic.CompareAndSwapInt64(&r.isDoomed, 0, 1)
}

// RefCount returns the current refcount.
func (r *Response) RefCount() int64 {
	return atomic.LoadInt64(&r.refCount)
}

// IncRefCount increments the refcount.
func (r *Response) IncRefCount() int64 {
	return atomic.AddInt64(&r.refCount, 1)
}

// DecRefCount decrements the refcount.
func (r *Response) DecRefCount() int64 {
	return atomic.AddInt64(&r.refCount, -1)
}

// CASRefCount performs a CAS on the refcount.
func (r *Response) CASRefCount(old, new int64) bool {
	return atomic.CompareAndSwapInt64(&r.refCount, old, new)
}

// StoreRefCount stores a new refcount value directly.
func (r *Response) StoreRefCount(new int64) {
	atomic.StoreInt64(&r.refCount, new)
}

// ShardListElement returns the LRU list element (for cache eviction).
func (r *Response) ShardListElement() any {
	return r.lruListElem.Load()
}

// clear resets the Response for re-use from the pool.
func (r *Response) clear() *Response {
	r.weight = 0
	r.refCount = 0
	r.isDoomed = 0
	r.revalidator = nil
	r.revalidatedAt = 0
	r.lruListElem.Store(nil)
	r.request.Store(nil)
	r.data.Store(nil)
	return r
}

var (
	respIsNilErr      = errors.New("response is nil")
	tooManyReadersErr = errors.New("too many readers")
)

func (r *Response) Close() error {
	if r == nil {
		return respIsNilErr
	}
	for {
		// Atomically decrement refCount. If the value is doomed and refCount drops to zero, actually release it.
		if old := r.RefCount(); r.CASRefCount(old, old-1) {
			if r.IsDoomed() && old == 1 {
				r.release()
				return nil
			}
			return tooManyReadersErr
		} else {
			continue
		}
	}
}

func (r *Response) release() {
	r.data.Load().Release()
	r.request.Load().Release()

	el := r.lruListElem.Load()
	if el != nil {
		// releases resources inside
		el.List().Remove(el)
	}

	r.clear()
	responsesPool.Put(r)
}
