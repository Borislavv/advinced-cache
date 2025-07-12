package list

import (
	"math/rand"
	"sync"
	"testing"
	"time"
)

// dummySized implements types.Sized for testing.
type dummySized struct {
	id int
}

func (d dummySized) Weight() int64 { return int64(d.id) }

func TestList_ConcurrentOperations(t *testing.T) {
	l := New[dummySized]()

	const numGoroutines = 16
	const numOpsPerGoroutine = 1000

	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(id)))
			var elements []*Element[dummySized]

			for j := 0; j < numOpsPerGoroutine; j++ {
				op := rng.Intn(4)

				switch op {
				case 0:
					e := l.PushFront(dummySized{id: id*numOpsPerGoroutine + j})
					elements = append(elements, e)
				case 1:
					e := l.PushBack(dummySized{id: id*numOpsPerGoroutine + j})
					elements = append(elements, e)
				case 2:
					if len(elements) > 0 {
						idx := rng.Intn(len(elements))
						l.MoveToFront(elements[idx])
					}
				case 3:
					if len(elements) > 0 {
						idx := rng.Intn(len(elements))
						l.Remove(elements[idx])
						elements = append(elements[:idx], elements[idx+1:]...)
					}
				}

				if l.Len() > 0 && rng.Intn(10) == 0 {
					_, _ = l.Next(0)
					_, _ = l.Next(l.Len() - 1)
				}
			}

			if l.Len() < 0 {
				t.Errorf("List length went negative")
			}
		}(i)
	}

	wg.Wait()

	// Verify ring integrity: count must match Len()
	count := 0
	e := l.root.next
	for e != l.root {
		count++
		if count > l.Len() {
			t.Fatalf("Cycle detected or broken links: count %d > Len() %d", count, l.Len())
		}
		e = e.next
	}
	if count != l.Len() {
		t.Errorf("Walked count %d != Len() %d", count, l.Len())
	}

	// Next() out-of-range should return false
	if _, ok := l.Next(l.Len()); ok {
		t.Errorf("Next() returned ok for out-of-range offset")
	}

	// Remove all elements and verify empty
	e = l.root.next
	for e != l.root {
		next := e.next
		l.Remove(e)
		e = next
	}
	if l.Len() != 0 {
		t.Errorf("List should be empty after full removal, got Len() = %d", l.Len())
	}

	// Refill with known order and test Sort()
	for i := 10; i >= 1; i-- {
		l.PushFront(dummySized{id: i})
	}

	// Verify initial order is descending
	var values []int
	e = l.root.next
	for e != l.root {
		values = append(values, e.value.id)
		e = e.next
	}
	if values[0] >= values[len(values)-1] {
		t.Errorf("Expected ascending order before Sort, got %+v", values)
	}

	// Sort ascending by Weight
	l.Sort(ASC)

	// Verify sorted order
	e = l.root.next
	lastID := -1
	for e != l.root {
		if int(e.value.Weight()) < lastID {
			t.Errorf("List is not sorted ascending: %d before %d", lastID, e.value.Weight())
		}
		lastID = int(e.value.Weight())
		e = e.next
	}

	// Verify ring integrity after Sort
	count = 0
	e = l.root.next
	for e != l.root {
		count++
		if count > l.Len() {
			t.Fatalf("Broken links or cycle detected after Sort: count %d > Len() %d", count, l.Len())
		}
		e = e.next
	}
	if count != l.Len() {
		t.Errorf("After Sort: walked count %d != Len() %d", count, l.Len())
	}
}
