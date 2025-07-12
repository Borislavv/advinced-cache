package lfu

import (
	"math/rand/v2"
	"sync/atomic"
)

const (
	sketchDepth = 4
	sketchWidth = 1 << 17 // 131K
)

type countMinSketch struct {
	table [sketchDepth][sketchWidth]uint32
	seeds [sketchDepth]uint64
}

func newCountMinSketch() *countMinSketch {
	c := &countMinSketch{}
	for i := 0; i < sketchDepth; i++ {
		c.seeds[i] = rand.Uint64()
	}
	return c
}

func (c *countMinSketch) Increment(key uint64) {
	for i := 0; i < sketchDepth; i++ {
		h := hash64(c.seeds[i], key)
		idx := h % sketchWidth
		atomic.AddUint32(&c.table[i][idx], 1)
	}
}

func (c *countMinSketch) estimate(key uint64) uint32 {
	mins := ^uint32(0)
	for i := 0; i < sketchDepth; i++ {
		h := hash64(c.seeds[i], key)
		idx := h % sketchWidth
		val := atomic.LoadUint32(&c.table[i][idx])
		if val < mins {
			mins = val
		}
	}
	return mins
}

func hash64(seed, key uint64) uint64 {
	x := key ^ seed
	x ^= x >> 33
	x *= 0xff51afd7ed558ccd
	x ^= x >> 33
	x *= 0xc4ceb9fe1a85ec53
	x ^= x >> 33
	return x
}
