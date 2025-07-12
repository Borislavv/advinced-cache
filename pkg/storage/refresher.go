package storage

import (
	"context"
	"github.com/Borislavv/advanced-cache/pkg/rate"
	"github.com/Borislavv/advanced-cache/pkg/storage/lru"
	"runtime"
	"strconv"
	"time"

	"github.com/Borislavv/advanced-cache/pkg/config"
	"github.com/Borislavv/advanced-cache/pkg/model"
	"github.com/Borislavv/advanced-cache/pkg/utils"
	"github.com/rs/zerolog/log"
)

var numProducers = runtime.GOMAXPROCS(0)

type Refresher interface {
	Run()
}

// Refresh is responsible for background refreshing of cache entries.
// It periodically samples random shards and randomly selects "cold" entries
// (from the end of each shard's Storage list) to refreshItem if necessary.
// Communication: provider->consumer (MPSC).
type Refresh struct {
	ctx                 context.Context
	cfg                 *config.Cache
	balancer            lru.Balancer
	storage             Storage
	rateLogCh           chan int
	refreshSuccessNumCh chan struct{}
	refreshErroredNumCh chan struct{}
	refreshItemsCh      chan *model.Entry
}

// NewRefresher constructs a Refresh.
func NewRefresher(ctx context.Context, cfg *config.Cache, balancer lru.Balancer, storage Storage) *Refresh {
	scanRate := cfg.Cache.Refresh.Rate * 10
	if scanRate < 1 {
		scanRate = 1
	}
	scanBurst := cfg.Cache.Refresh.Rate / 10
	if scanBurst < 1 {
		scanBurst = 1
	}

	return &Refresh{
		ctx:                 ctx,
		cfg:                 cfg,
		storage:             storage,
		balancer:            balancer,
		rateLogCh:           make(chan int, cfg.Cache.Refresh.Rate),
		refreshSuccessNumCh: make(chan struct{}, cfg.Cache.Refresh.Rate),     // Successful refreshes counter channel
		refreshErroredNumCh: make(chan struct{}, cfg.Cache.Refresh.Rate),     // Failed refreshes counter channel
		refreshItemsCh:      make(chan *model.Entry, cfg.Cache.Refresh.Rate), // Failed refreshes counter channel
	}
}

// Run starts the refresher background loop.
// It runs a logger (if debugging is enabled), spawns a provider for sampling shards,
// and continuously processes shard samples for candidate responses to refreshItem.
func (r *Refresh) Run() {
	r.runLogger()    // handle consumer stats and print logs
	r.runConsumers() // scans rand items and checks whether they should be refreshed
	r.runProducers() // produces items which should be refreshed on processing
}

func (r *Refresh) runProducers() {
	scanRate := r.cfg.Cache.Refresh.ScanRate / numProducers
	if scanRate < 1 {
		scanRate = 1
	}
	scanRateBurst := scanRate / 10 // burst is 10% of total rate
	if scanRateBurst < 1 {
		scanRateBurst = 1
	}
	for i := 0; i < numProducers; i++ {
		r.runProducer(scanRate, scanRateBurst)
	}
}

func (r *Refresh) runProducer(scanRate, scanBurst int) {
	go func() {
		ctx, cancel := context.WithTimeout(r.ctx, time.Second*10)
		defer cancel()

		scansRateLimiter := rate.NewLimiter(ctx, scanRate, scanBurst)

		for {
			select {
			case <-ctx.Done():
				actualRate := int((float64(r.storage.Len()) / r.cfg.Cache.Refresh.TTL.Seconds()) / float64(numProducers))
				if actualRate > r.cfg.Cache.Refresh.Rate {
					actualRate = r.cfg.Cache.Refresh.Rate
				} else if actualRate < 1 {
					actualRate = 1
				}

				actualBurst := actualRate / 10
				if actualBurst < 1 {
					actualBurst = 1
				}

				r.runProducer(actualRate*10, actualBurst*10)
				return
			case <-scansRateLimiter.Chan():
				if item := r.balancer.RandNode().RandItem(ctx); item.ShouldBeRefreshed(r.cfg) {
					r.refreshItemsCh <- item
				}
			}
		}
	}()
}

func (r *Refresh) runConsumers() {
	reqRate := r.cfg.Cache.Refresh.Rate
	if reqRate < 1 {
		reqRate = 1
	}
	reqRateBurst := reqRate / 10 // burst is 10% of total rate
	if reqRateBurst < 1 {
		reqRateBurst = 1
	}
	r.runConsumer(reqRate, reqRateBurst)
}

func (r *Refresh) runConsumer(reqRate, reqBurst int) {
	go func() {
		ctx, cancel := context.WithTimeout(r.ctx, time.Minute)
		defer cancel()

		requestsRateLimiter := rate.NewLimiter(ctx, reqRate, reqBurst)

		for entry := range r.refreshItemsCh {
			select {
			case <-ctx.Done():
				actualRate := int(float64(r.storage.Len()) / r.cfg.Cache.Refresh.TTL.Seconds())
				if actualRate > r.cfg.Cache.Refresh.Rate {
					select {
					case r.rateLogCh <- actualRate:
					}
					actualRate = r.cfg.Cache.Refresh.Rate
				} else if actualRate < 1 {
					actualRate = 1
				}

				actualBurst := actualRate / 10
				if actualBurst < 1 {
					actualBurst = 1
				}

				r.runConsumer(actualRate, actualBurst)
				return
			case <-requestsRateLimiter.Chan():
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
			case actualRate := <-r.rateLogCh:
				log.Warn().Msgf(
					"[refresher] configured rate limit is %d per second "+
						"but actually for have been able refresh all keys per TTL an appropriate value is %d per second",
					r.cfg.Cache.Refresh.Rate, actualRate,
				)
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
