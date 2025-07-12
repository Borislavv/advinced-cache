package lfu

import (
	"context"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/Borislavv/advanced-cache/pkg/model"
)

func TestTinyLFUConcurrentUsage(t *testing.T) {
	tlfu := NewTinyLFU(context.Background())

	var wg sync.WaitGroup

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < 1_000_000; j++ {
				key := uint64(rand.Int63())
				tlfu.Increment(key)
			}
		}(i)
	}

	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			newEntry := &model.Entry{}
			evictEntry := &model.Entry{}
			for j := 0; j < 1_000_000; j++ {
				newEntry.SetMapKey(uint64(rand.Int63()))
				evictEntry.SetMapKey(uint64(rand.Int63()))
				tlfu.Admit(newEntry, evictEntry)
			}
		}(i)
	}

	time.Sleep(2 * time.Second)

	wg.Wait()
}
