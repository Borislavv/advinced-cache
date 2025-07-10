package storage

import (
	"context"
	"github.com/Borislavv/advanced-cache/pkg/rate"
	"github.com/Borislavv/advanced-cache/pkg/storage/lru"
	"strconv"
	"time"

	"github.com/Borislavv/advanced-cache/pkg/config"
	"github.com/Borislavv/advanced-cache/pkg/model"
	"github.com/Borislavv/advanced-cache/pkg/utils"
	"github.com/rs/zerolog/log"
)

type Refresher interface {
	Run()
}

// Refresh is responsible for background refreshing of cache entries.
// It periodically samples random shards and randomly selects "cold" entries
// (from the end of each shard's Storage list) to refreshItem if necessary.
type Refresh struct {
	ctx                 context.Context
	cfg                 *config.Cache
	balancer            lru.Balancer
	scansRateLimiter    *rate.Limiter
	requestRateLimiter  *rate.Limiter
	refreshSuccessNumCh chan struct{}
	refreshErroredNumCh chan struct{}
	refreshItemsCh      chan *model.Entry
}

// NewRefresher constructs a Refresh.
func NewRefresher(ctx context.Context, cfg *config.Cache, balancer lru.Balancer) *Refresh {
	return &Refresh{
		ctx:                 ctx,
		cfg:                 cfg,
		balancer:            balancer,
		scansRateLimiter:    rate.NewLimiter(ctx, cfg.Cache.Refresh.ScanRate, cfg.Cache.Refresh.ScanRate/10),
		requestRateLimiter:  rate.NewLimiter(ctx, cfg.Cache.Refresh.Rate, cfg.Cache.Refresh.Rate/10),
		refreshSuccessNumCh: make(chan struct{}, cfg.Cache.Refresh.Rate),     // Successful refreshes counter channel
		refreshErroredNumCh: make(chan struct{}, cfg.Cache.Refresh.Rate),     // Failed refreshes counter channel
		refreshItemsCh:      make(chan *model.Entry, cfg.Cache.Refresh.Rate), // Failed refreshes counter channel
	}
}

// Run starts the refresher background loop.
// It runs a logger (if debugging is enabled), spawns a provider for sampling shards,
// and continuously processes shard samples for candidate responses to refreshItem.
func (r *Refresh) Run() {
	return
	r.runLogger()   // handle consumer stats and print logs
	r.runConsumer() // scans rand items and checks whether they should be refreshed
	r.runProducer() // produces items which should be refreshed on processing
}

func (r *Refresh) runProducer() {
	go func() {
		for {
			select {
			case <-r.ctx.Done():
				return
			case <-r.scansRateLimiter.Chan():
				if item := r.balancer.RandNode().RandItem(r.ctx); item.ShouldBeRefreshed(r.cfg) {
					r.refreshItemsCh <- item
				}
			}
		}
	}()
}

func (r *Refresh) runConsumer() {
	go func() {
		for entry := range r.refreshItemsCh {
			select {
			case <-r.ctx.Done():
				return
			case <-r.requestRateLimiter.Chan():
				go func() {
					if err := entry.Revalidate(); err != nil {
						r.refreshErroredNumCh <- struct{}{}
						return
					}
					r.refreshSuccessNumCh <- struct{}{}
				}()
			}
		}
	}()
}

// runLogger periodically logs the number of successful and failed refreshItem attempts.
// This runs only if debugging is enabled in the config.
func (r *Refresh) runLogger() {
	go func() {
		erroredNumPer5Sec := 0
		refreshesNumPer5Sec := 0
		ticker := utils.NewTicker(r.ctx, 5*time.Second)

	loop:
		for {
			select {
			case <-r.ctx.Done():
				return
			case <-r.refreshSuccessNumCh:
				refreshesNumPer5Sec++
			case <-r.refreshErroredNumCh:
				erroredNumPer5Sec++
			case <-ticker:
				if refreshesNumPer5Sec <= 0 && erroredNumPer5Sec <= 0 {
					continue loop
				}

				var (
					errorsNum  = strconv.Itoa(erroredNumPer5Sec)
					successNum = strconv.Itoa(refreshesNumPer5Sec)
				)

				logEvent := log.Info()

				if r.cfg.IsProd() {
					logEvent.
						Str("target", "refresher").
						Str("refreshes", successNum).
						Str("errors", errorsNum)
				}

				logEvent.Msgf("[refresher][5s] updated %s items, errors: %s", successNum, errorsNum)

				refreshesNumPer5Sec = 0
				erroredNumPer5Sec = 0
			}
		}
	}()
}
