package resource

import (
	"sync"
	"unsafe"
)

const defaultBodyLength = 1024

var bodyPool = &sync.Pool{
	New: func() any {
		return &ReleasableBody{
			p: make([]byte, 0, defaultBodyLength),
		}
	},
}

type ReleasableBody struct {
	p []byte
}

func AcquireBody() *ReleasableBody {
	return bodyPool.Get().(*ReleasableBody)
}

// Bytes - returns a slice bytes which manages under sync.Pool. Copy output slice, don't use the same value.
func (r *ReleasableBody) Bytes() []byte {
	return r.p
}

func (r *ReleasableBody) Weight() int64 {
	return int64(cap(r.p)) + int64(unsafe.Sizeof(*r))
}

func (r *ReleasableBody) Reset() {
	r.p = r.p[:0]
}

func (r *ReleasableBody) Append(p []byte) *ReleasableBody {
	r.p = append(r.p, p...)
	return r
}

func (r *ReleasableBody) Release() {
	r.Reset()
	bodyPool.Put(r)
}

// Releasable defines reference-counted resource management for cache values.
type Releasable interface {
	Close() error
	RefCount() int64
	IncRefCount() int64
	DecRefCount() int64
	CASRefCount(old, new int64) bool
	StoreRefCount(new int64)
	IsDoomed() bool
	MarkAsDoomed() bool
	ShardListElement() any // Used for pointer to the element in the LRU or similar
}
