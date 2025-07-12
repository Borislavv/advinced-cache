package lfu

import (
	"math/rand/v2"
	"sync/atomic"
)

type doorkeeper struct {
	bits  []uint64
	seeds [2]uint64
	mask  uint64
}

func newDoorkeeper(capacity int) *doorkeeper {
	size := capacity / 64
	return &doorkeeper{
		bits:  make([]uint64, size),
		seeds: [2]uint64{rand.Uint64(), rand.Uint64()},
		mask:  uint64(size*64 - 1),
	}
}

func (d *doorkeeper) Allow(key uint64) bool {
	h1 := hash64(d.seeds[0], key) & d.mask
	h2 := hash64(d.seeds[1], key) & d.mask

	b1 := (atomic.LoadUint64(&d.bits[h1/64]) & (1 << (h1 % 64))) != 0
	b2 := (atomic.LoadUint64(&d.bits[h2/64]) & (1 << (h2 % 64))) != 0

	if b1 && b2 {
		return true
	}

	atomic.OrUint64(&d.bits[h1/64], 1<<(h1%64))
	atomic.OrUint64(&d.bits[h2/64], 1<<(h2%64))
	return false
}
