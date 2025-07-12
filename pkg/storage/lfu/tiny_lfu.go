package lfu

import (
	"context"
	"github.com/Borislavv/advanced-cache/pkg/model"
	"sync/atomic"
	"time"
)

const doorkeeperCapacity = 1 << 19

type TinyLFU struct {
	curr atomic.Pointer[countMinSketch]
	prev atomic.Pointer[countMinSketch]
	door *doorkeeper
}

func NewTinyLFU(ctx context.Context) *TinyLFU {
	lfu := &TinyLFU{door: newDoorkeeper(doorkeeperCapacity)}
	lfu.curr.Store(newCountMinSketch())
	lfu.prev.Store(newCountMinSketch())
	go lfu.run(ctx)
	return lfu
}

func (t *TinyLFU) run(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.Rotate()
		}
	}
}

func (t *TinyLFU) Increment(key uint64) {
	t.curr.Load().Increment(key)
	t.door.Allow(key)
}

func (t *TinyLFU) Admit(new, old *model.Entry) bool {
	newKey := new.MapKey()
	oldKey := old.MapKey()

	if !t.door.Allow(newKey) {
		return true
	}

	newFreq := t.estimate(newKey)
	oldFreq := t.estimate(oldKey)

	return newFreq >= oldFreq
}

func (t *TinyLFU) Rotate() {
	// current -> previous
	curr := t.curr.Load()
	t.prev.Store(curr)

	// current -> new current (new seeds)
	t.curr.Store(newCountMinSketch())

	// doorkeeper -> new doorkeeper
	t.door = newDoorkeeper(doorkeeperCapacity)
}

func (t *TinyLFU) estimate(key uint64) uint32 {
	c := t.curr.Load().estimate(key)
	p := t.prev.Load().estimate(key)
	return (c + p) / 2
}
