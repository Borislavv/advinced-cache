package rate

import (
	"context"
	"golang.org/x/time/rate"
)

type Limiter struct {
	ch chan struct{}
	*rate.Limiter
}

func NewLimiter(ctx context.Context, limit, burst int) *Limiter {
	l := &Limiter{
		ch:      make(chan struct{}),
		Limiter: rate.NewLimiter(rate.Limit(limit), burst),
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				close(l.ch)
				return
			default:
				if err := l.Wait(ctx); err != nil {
					return
				}

				select {
				case <-ctx.Done():
					return
				case l.ch <- struct{}{}:
				}
			}
		}
	}()
	return l
}

func (l *Limiter) Chan() <-chan struct{} {
	return l.ch
}
