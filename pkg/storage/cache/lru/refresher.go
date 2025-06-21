package lru

import (
	"context"
	"github.com/Borislavv/traefik-http-cache-plugin/pkg/rate"
	"runtime"
	"strconv"
	"time"

	"github.com/Borislavv/traefik-http-cache-plugin/pkg/config"
	"github.com/Borislavv/traefik-http-cache-plugin/pkg/model"
	"github.com/Borislavv/traefik-http-cache-plugin/pkg/utils"
	"github.com/rs/zerolog/log"
)

const (
	shardRateLimit        = 64   // Global limiter: maximum concurrent refreshes across all shards
	shardRateLimitBurst   = 16   // Global limiter: maximum parallel requests.
	refreshRateLimit      = 1024 // Global limiter: maximum concurrent refreshes across all shards
	refreshRateLimitBurst = 128  // Global limiter: maximum parallel requests.
	refreshSamples        = 16   // Number of items to sample per shard per refreshItem tick
	// Max refreshes per second = shardRateLimit(32) * refreshSamples(32) = 1024.
)

var (
	refreshSuccessNumCh = make(chan struct{}, runtime.GOMAXPROCS(0)*4) // Successful refreshes counter channel
	refreshErroredNumCh = make(chan struct{}, runtime.GOMAXPROCS(0)*4) // Failed refreshes counter channel
)

type Refresher interface {
	RunRefresher()
}

// Refresh is responsible for background refreshing of cache entries.
// It periodically samples random shards and randomly selects "cold" entries
// (from the end of each shard's Storage list) to refreshItem if necessary.
type Refresh struct {
	ctx                context.Context
	cfg                *config.Cache
	balancer           Balancer
	shardRateLimiter   *rate.Limiter
	refreshRateLimiter *rate.Limiter
}

// NewRefresher constructs a Refresh.
func NewRefresher(ctx context.Context, cfg *config.Cache, balancer Balancer) *Refresh {
	return &Refresh{
		ctx:                ctx,
		cfg:                cfg,
		balancer:           balancer,
		shardRateLimiter:   rate.NewLimiter(ctx, shardRateLimit, shardRateLimitBurst),
		refreshRateLimiter: rate.NewLimiter(ctx, refreshRateLimit, refreshRateLimitBurst),
	}
}

// RunRefresher starts the refresher background loop.
// It runs a logger (if debugging is enabled), spawns a provider for sampling shards,
// and continuously processes shard samples for candidate responses to refreshItem.
func (r *Refresh) RunRefresher() {
	go func() {
		r.runLogger()
		for {
			select {
			case <-r.ctx.Done():
				return
			case <-r.shardRateLimiter.Chan(): // Throttling (64 per second)
				r.refreshNode(r.balancer.RandShardNode())
			}
		}
	}()
}

// refreshNode selects up to refreshSamples entries from the end of the given shard's Storage list.
// For each candidate, if ShouldBeRefreshed() returns true and shardRateLimiter limiting allows, triggers an asynchronous refreshItem.
func (r *Refresh) refreshNode(node *ShardNode) {
	ctx, cancel := context.WithTimeout(r.ctx, time.Second)
	defer cancel()

	samples := 0
	node.shard.Walk(ctx, func(key uint64, resp *model.Response) bool {
		if samples >= refreshSamples {
			return false
		}
		if resp.ShouldBeRefreshed() {
			select {
			case <-ctx.Done():
				return false
			case <-r.refreshRateLimiter.Chan(): // Throttling (1024 per second)
				go r.refreshItem(resp)
				samples++
			}
		}
		return true
	}, false)
}

// refreshItem attempts to refreshItem the given response via Revalidate.
// If successful, increments the refreshItem metric (in debug mode); otherwise increments the error metric.
func (r *Refresh) refreshItem(resp *model.Response) {
	if err := resp.Revalidate(r.ctx); err != nil {
		select {
		case refreshErroredNumCh <- struct{}{}:
		default:
			runtime.Gosched()
		}
		return
	}
	select {
	case refreshSuccessNumCh <- struct{}{}:
	default:
		runtime.Gosched()
	}
}

// runLogger periodically logs the number of successful and failed refreshItem attempts.
// This runs only if debugging is enabled in the config.
func (r *Refresh) runLogger() {
	go func() {
		erroredNumPer5Sec := 0
		refreshesNumPer5Sec := 0
		ticker := utils.NewTicker(r.ctx, 5*time.Second)
		for {
			select {
			case <-r.ctx.Done():
				return
			case <-refreshSuccessNumCh:
				refreshesNumPer5Sec++
				runtime.Gosched()
			case <-refreshErroredNumCh:
				erroredNumPer5Sec++
				runtime.Gosched()
			case <-ticker:
				var (
					errorsNum  = strconv.Itoa(erroredNumPer5Sec)
					successNum = strconv.Itoa(refreshesNumPer5Sec)
				)

				log.
					Info().
					//Str("target", "refresher").
					//Str("processed", successNum).
					//Str("errored", errorsNum).
					Msgf("[refresher][5s] success %s, errors: %s", successNum, errorsNum)

				refreshesNumPer5Sec = 0
				erroredNumPer5Sec = 0
				runtime.Gosched()
			}
		}
	}()
}
