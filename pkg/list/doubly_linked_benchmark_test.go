package list

import (
	"math/rand"
	"testing"
	"time"
)

// BenchmarkList_PushFrontParallel checks throughput for PushFront under parallel load.
func BenchmarkList_PushFrontParallel(b *testing.B) {
	l := New[dummySized]()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			l.PushFront(dummySized{id: rand.Int()})
		}
	})
}

// BenchmarkList_MoveToFrontParallel checks throughput for MoveToFront with mixed elements.
func BenchmarkList_MoveToFrontParallel(b *testing.B) {
	l := New[dummySized]()

	// Pre-populate list
	const numElements = 1000
	var elements []*Element[dummySized]
	for i := 0; i < numElements; i++ {
		elements = append(elements, l.PushFront(dummySized{id: i}))
	}

	b.RunParallel(func(pb *testing.PB) {
		rng := rand.New(rand.NewSource(time.Now().UnixNano()))
		for pb.Next() {
			idx := rng.Intn(len(elements))
			l.MoveToFront(elements[idx])
		}
	})
}

// BenchmarkList_RemoveParallel checks throughput for Remove with mixed elements.
func BenchmarkList_SetAndRemoveAtTheSameTimeParallel(b *testing.B) {
	l := New[dummySized]()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			e := l.PushFront(dummySized{id: rand.Int()})
			l.Remove(e)
		}
	})
}
