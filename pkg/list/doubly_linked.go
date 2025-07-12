package list

import (
	"sync"
	"sync/atomic"

	"github.com/Borislavv/advanced-cache/pkg/types"
)

// Element is a reusable node for the doubly linked list.
type Element[T types.Sized] struct {
	next, prev *Element[T]
	list       *List[T]
	value      T
}

func (e *Element[T]) Prev() *Element[T] { return e.prev }
func (e *Element[T]) Next() *Element[T] { return e.next }
func (e *Element[T]) List() *List[T]    { return e.list }
func (e *Element[T]) Value() T          { return e.value }
func (e *Element[T]) Weight() int64     { return e.value.Weight() }

// List is a shard-local doubly linked list for LRU order.
type List[T types.Sized] struct {
	mu   sync.Mutex
	root *Element[T]
	len  int64
	pool sync.Pool
}

// New creates a new List with its own mutex.
func New[T types.Sized]() *List[T] {
	l := &List[T]{
		pool: sync.Pool{ // sync.Pool for Element reuse.
			New: func() any { return new(Element[T]) },
		},
	}
	root := l.newElement()
	l.root = root
	root.next, root.prev = root, root
	return l
}

func (l *List[T]) newElement() *Element[T] {
	return l.pool.Get().(*Element[T])
}

func (l *List[T]) freeElement(e *Element[T]) {
	e.next, e.prev, e.list = nil, nil, nil
	var zero T
	e.value = zero
	l.pool.Put(e)
}

// Len returns the length, safe for concurrent read.
func (l *List[T]) Len() int {
	return int(atomic.LoadInt64(&l.len))
}

// PushFront adds v at the front and returns the element.
func (l *List[T]) PushFront(v T) *Element[T] {
	l.mu.Lock()
	defer l.mu.Unlock()

	e := l.newElement()
	e.value = v
	e.list = l

	at := l.root
	e.prev = at
	e.next = at.next
	at.next.prev = e
	at.next = e

	atomic.AddInt64(&l.len, 1)
	return e
}

// PushBack adds v at the back and returns the element.
func (l *List[T]) PushBack(v T) *Element[T] {
	l.mu.Lock()
	defer l.mu.Unlock()

	e := l.newElement()
	e.value = v
	e.list = l

	at := l.root.prev
	e.prev = at
	e.next = l.root
	at.next = e
	l.root.prev = e

	atomic.AddInt64(&l.len, 1)
	return e
}

// Back returns the last element or nil if empty.
func (l *List[T]) Back() *Element[T] {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.Len() == 0 {
		return nil
	}
	return l.root.prev
}

// Remove deletes e from the list.
func (l *List[T]) Remove(e *Element[T]) {
	if e == nil || e.list != l {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	e.prev.next = e.next
	e.next.prev = e.prev

	atomic.AddInt64(&l.len, -1)
	l.freeElement(e)
}

// MoveToFront moves e to the front.
func (l *List[T]) MoveToFront(e *Element[T]) {
	if e == nil || e.list != l || l.Len() < 2 {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	// Detach
	e.prev.next = e.next
	e.next.prev = e.prev

	// Insert after root
	e.prev = l.root
	e.next = l.root.next
	l.root.next.prev = e
	l.root.next = e
}

// Next returns the element at offset from front.
func (l *List[T]) Next(offset int) (*Element[T], bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if offset < 0 || offset >= l.Len() {
		return nil, false
	}

	e := l.root.next
	for i := 0; i < offset; i++ {
		e = e.next
	}
	return e, true
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

func mergeSortByWeight[T types.Sized](head *Element[T], ord Order) *Element[T] {
	if head == nil || head.next == nil {
		return head
	}

	mid := splitHalf(head)

	left := mergeSortByWeight(mid, ord)
	right := mergeSortByWeight(head, ord)

	return mergeByWeight(left, right, ord)
}

func splitHalf[T types.Sized](head *Element[T]) *Element[T] {
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

func mergeByWeight[T types.Sized](a, b *Element[T], ord Order) *Element[T] {
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
