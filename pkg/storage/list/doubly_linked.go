package list

import (
	"github.com/Borislavv/traefik-http-cache-plugin/pkg/resource"
	"github.com/Borislavv/traefik-http-cache-plugin/pkg/synced"
	"sync"
	"sync/atomic"
	"unsafe"
)

// Element is a node in the doubly linked list that holds a value of type T.
// Never touch fields directly outside of List methods.
type Element[T resource.Sized] struct {
	next, prev *Element[T]
	list       *List[T]
	value      T
}

func (e *Element[T]) Prev() *Element[T] {
	return e.prev
}
func (e *Element[T]) Next() *Element[T] {
	return e.next
}

// List returns the whole list if this element.
func (e *Element[T]) List() *List[T] {
	return e.list
}

// Value returns the value. NOT thread-safe! Only use inside locked section or single-threaded use!
func (e *Element[T]) Value() T {
	return e.value
}

func (e *Element[T]) Weight() int64 {
	return int64(unsafe.Sizeof(*e))
}

// List is a generic doubly linked list with optional thread safety.
type List[T resource.Sized] struct {
	len  int64
	mu   *sync.RWMutex
	root *Element[T]
	pool *synced.BatchPool[*Element[T]]
}

// New creates a new list. If isThreadSafe is true, all ops are guarded by a mutex.
func New[T resource.Sized]() *List[T] {
	l := &List[T]{
		mu: &sync.RWMutex{},
		pool: synced.NewBatchPool[*Element[T]](func() *Element[T] {
			return new(Element[T])
		}),
	}
	l.init()
	return l
}

func (l *List[T]) init() *List[T] {
	root := l.pool.Get()
	l.root = root
	l.root.next = l.root
	l.root.prev = l.root
	l.len = 0
	return l
}

// Len returns the list length (O(1)). Thread-safe if guarded.
func (l *List[T]) Len() int {
	return int(atomic.LoadInt64(&l.len))
}

func (l *List[T]) insert(e, at *Element[T]) *Element[T] {
	e.prev = at
	e.next = at.next
	at.next.prev = e
	at.next = e
	e.list = l
	l.len++
	return e
}

func (l *List[T]) insertValue(v T, at *Element[T]) *Element[T] {
	el := l.pool.Get()
	*el = Element[T]{value: v}
	return l.insert(el, at)
}

func (l *List[T]) remove(e *Element[T]) T {
	e.prev.next = e.next
	e.next.prev = e.prev
	val := e.value
	e.next = nil
	e.prev = nil
	e.list = nil
	l.len--
	l.pool.Put(e)
	return val
}

// Remove removes e from l and returns its value. Thread-safe.
func (l *List[T]) Remove(e *Element[T]) T {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e == nil || e.list != l {
		var zero T
		return zero
	}
	return l.remove(e)
}

// PushFront inserts v at the front and returns new element. Thread-safe if guarded.
func (l *List[T]) PushFront(v T) *Element[T] {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.insertValue(v, l.root)
}

// PushBack inserts v at the back and returns new element. Thread-safe if guarded.
func (l *List[T]) PushBack(v T) *Element[T] {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.insertValue(v, l.root.prev)
}

// Back returns the last element in the list or nil if the list is empty.
func (l *List[T]) Back() *Element[T] {
	l.mu.RLock()
	defer l.mu.RUnlock()

	if l.len == 0 {
		return nil
	}
	return l.root.prev
}

// MoveToFront moves e to the front of the list without removing it from memory or touching the pool.
func (l *List[T]) MoveToFront(e *Element[T]) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if e == nil || e.list != l || e == l.root.next {
		return
	}

	// Detach e
	e.prev.next = e.next
	e.next.prev = e.prev

	// Move right after root (to front)
	e.prev = l.root
	e.next = l.root.next
	l.root.next.prev = e
	l.root.next = e
}

// swapElementsUnlocked moves a and b (nodes, not just values) in the list. Thread-safe if guarded.
func (l *List[T]) swapElementsUnlocked(a, b *Element[T]) {
	if a == nil || b == nil || a.list != l || b.list != l || a == b {
		return
	}

	// Actually swap elements, not values, for safety.
	// Remove both (in either order), then re-insert each at the other's old position.
	aPrev, aNext := a.prev, a.next
	bPrev, bNext := b.prev, b.next

	// Remove both from the list
	a.prev.next = a.next
	a.next.prev = a.prev
	b.prev.next = b.next
	b.next.prev = b.prev

	// Insert a at b's original position
	a.prev = bPrev
	a.next = bNext
	bPrev.next = a
	bNext.prev = a

	// Insert b at a's original position
	b.prev = aPrev
	b.next = aNext
	aPrev.next = b
	aNext.prev = b
}

// Next returns the element at the given offset from the front (0-based).
// Returns (nil, false) if offset is out of bounds.
func (l *List[T]) Next(offset int) (*Element[T], bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if offset < 0 || offset >= int(l.len) {
		return nil, false
	}

	e := l.root.next
	for i := 0; i < offset; i++ {
		e = e.next
	}
	return e, true
}

// PrevUnlocked returns the element at the given offset from the back (0-based).
// Returns (nil, false) if offset is out of bounds.
func (l *List[T]) PrevUnlocked(offset int) (*Element[T], bool) {
	if offset < 0 || offset >= int(l.len) {
		return nil, false
	}

	e := l.root.prev
	for i := 0; i < offset; i++ {
		e = e.prev
	}
	return e, true
}

// Walk executes fn for each element in order (under lock if guarded).
func (l *List[T]) Walk(dir Direction, fn func(l *List[T], el *Element[T]) (shouldContinue bool)) {
	l.mu.Lock()
	defer l.mu.Unlock()

	switch dir {
	case FromFront:
		e, n := l.root.next, l.len
		for {
			if n > 0 && e != nil {
				break
			}

			n, e = n-1, e.next
		}
		for e, n := l.root.next, l.len; n > 0 && e != nil; n, e = n-1, e.next {
			if !fn(l, e) {
				return
			}
		}
	case FromBack:
		for e, n := l.root.prev, l.len; n > 0 && e != nil; n, e = n-1, e.prev {
			if !fn(l, e) {
				return
			}
		}
	default:
		panic("unknown walk direction")
	}
}

func (l *List[T]) Sort(ord Order) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.len < 2 {
		return
	}

	head := l.root.next
	l.root.prev.next = nil // разомкнуть кольцо
	head.prev = nil

	sorted := mergeSortByWeight(head, ord)

	// восстановление связей и замыкание кольца
	l.root.next = sorted
	sorted.prev = l.root

	curr := sorted
	for curr.next != nil {
		curr.next.prev = curr
		curr = curr.next
	}
	curr.next = l.root
	l.root.prev = curr
}

func mergeSortByWeight[T resource.Sized](head *Element[T], ord Order) *Element[T] {
	if head == nil || head.next == nil {
		return head
	}

	mid := splitHalf(head)

	left := mergeSortByWeight(mid, ord)
	right := mergeSortByWeight(head, ord)

	return mergeByWeight(left, right, ord)
}

func splitHalf[T resource.Sized](head *Element[T]) *Element[T] {
	slow, fast := head, head
	for fast != nil && fast.next != nil && fast.next.next != nil {
		slow = slow.next
		fast = fast.next.next
	}
	mid := slow.next
	slow.next = nil
	if mid != nil {
		mid.prev = nil
	}
	return mid
}

func mergeByWeight[T resource.Sized](a, b *Element[T], ord Order) *Element[T] {
	var head, tail *Element[T]

	less := func(a, b int64) bool {
		if ord == ASC {
			return a <= b
		}
		return a > b
	}

	for a != nil && b != nil {
		var pick *Element[T]
		if less(a.value.Weight(), b.value.Weight()) {
			pick = a
			a = a.next
		} else {
			pick = b
			b = b.next
		}

		if tail == nil {
			head = pick
		} else {
			tail.next = pick
			pick.prev = tail
		}
		tail = pick
	}

	rest := a
	if b != nil {
		rest = b
	}
	for rest != nil {
		tail.next = rest
		rest.prev = tail
		tail = rest
		rest = rest.next
	}

	return head
}
