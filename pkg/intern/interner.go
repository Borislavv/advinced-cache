package intern

import (
	"fmt"
	"sync"
	"unsafe"
)

var (
	PathInterner      = NewInterner(8)
	HeaderKeyInterner = NewInterner(8)
	QueryKeyInterner  = NewInterner(16)
)

// Interner is a simple threadsafe interner for []byte.
type Interner struct {
	mu   sync.RWMutex
	data map[string][]byte
}

func NewInterner(cap int) *Interner {
	return &Interner{
		data: make(map[string][]byte, cap),
	}
}

func (i *Interner) Print() {
	i.mu.RLock()
	defer i.mu.RUnlock()
	for k, v := range i.data {
		fmt.Printf("%s: %s\n", k, v)
	}
}

// Intern returns an interned []byte for the given slice.
// It assumes that the input []byte will not be modified after interning.
func (i *Interner) Intern(b []byte, shouldCopy bool) []byte {
	// Use zero-copy string as key.
	key := unsafe.String(unsafe.SliceData(b), len(b))

	i.mu.RLock()
	v, found := i.data[key]
	i.mu.RUnlock()
	if found {
		return v
	}

	var val []byte
	if shouldCopy {
		val = make([]byte, len(b))
		copy(val, b)
	} else {
		val = b
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	if existing, ok := i.data[key]; ok {
		return existing
	}
	i.data[key] = val
	return val
}

func (i *Interner) InternStr(s string) []byte {
	key := s

	i.mu.RLock()
	v, found := i.data[key]
	i.mu.RUnlock()
	if found {
		return v
	}

	copied := make([]byte, len(key))
	copy(copied, key)

	i.mu.Lock()
	defer i.mu.Unlock()
	if existing, ok := i.data[key]; ok {
		return existing
	}
	i.data[key] = copied
	return copied
}
