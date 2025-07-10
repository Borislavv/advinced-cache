package pools

import (
	"sync"
)

var (
	KeyValueSlicePool = sync.Pool{
		New: func() interface{} {
			return make([][2][]byte, 0, 64)
		},
	}
)
