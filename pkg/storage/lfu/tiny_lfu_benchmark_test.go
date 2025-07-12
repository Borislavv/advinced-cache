package lfu

import (
	"context"
	"github.com/Borislavv/advanced-cache/pkg/model"
	"math/rand"
	"testing"
	"time"
)

func BenchmarkTinyLFUIncrement(b *testing.B) {
	tlfu := NewTinyLFU(context.Background())

	keys := make([]uint64, b.N)
	for i := 0; i < b.N; i++ {
		keys[i] = rand.Uint64()
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tlfu.Increment(keys[i])
	}
}

func BenchmarkTinyLFUAdmit(b *testing.B) {
	tlfu := NewTinyLFU(context.Background())

	// simulate some initial frequencies
	for i := 0; i < 100000; i++ {
		tlfu.Increment(uint64(i))
	}
	time.Sleep(time.Second) // wait for run()

	newEntry := (&model.Entry{}).SetMapKey(rand.Uint64())
	oldEntry := (&model.Entry{}).SetMapKey(rand.Uint64())

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tlfu.Admit(newEntry, oldEntry)
	}
}
