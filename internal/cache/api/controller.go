package api

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/Borislavv/traefik-http-cache-plugin/internal/cache/config"
	"github.com/Borislavv/traefik-http-cache-plugin/pkg/model"
	"github.com/Borislavv/traefik-http-cache-plugin/pkg/repository"
	serverutils "github.com/Borislavv/traefik-http-cache-plugin/pkg/server/utils"
	"github.com/Borislavv/traefik-http-cache-plugin/pkg/storage"
	"github.com/Borislavv/traefik-http-cache-plugin/pkg/utils"
	"github.com/fasthttp/router"
	"github.com/rs/zerolog/log"
	"github.com/valyala/fasthttp"
	"net/http"
	"runtime"
	"strconv"
	"sync/atomic"
	"time"
)

// CacheGetPath for getting pagedata from cache via HTTP.
const CacheGetPath = "/{any:*}"

// Predefined HTTP response templates for error handling (400/503)
var (
	badRequestResponseBytes = []byte(`{
	  "status": 400,
	  "error": "Bad Request",
	  "message": "` + string(messagePlaceholder) + `"
	}`)
	serviceUnavailableResponseBytes = []byte(`{
	  "status": 503,
	  "error": "Service Unavailable",
	  "message": "` + string(messagePlaceholder) + `"
	}`)
	messagePlaceholder = []byte("${message}")
	zeroLiteral        = "0"
)

// Buffered channel for request durations (used only if debug enabled)
var (
	count    = &atomic.Int64{} // Num
	duration = &atomic.Int64{} // UnixNano
)

// CacheController handles cache API requests (read/write-through, error reporting, metrics).
type CacheController struct {
	cfg     *config.Config
	ctx     context.Context
	cache   storage.Storage
	backend repository.Backender
}

// NewCacheController builds a cache API controller with all dependencies.
// If debug is enabled, launches internal stats logger goroutine.
func NewCacheController(
	ctx context.Context,
	cfg *config.Config,
	cache storage.Storage,
	backend repository.Backender,
) *CacheController {
	c := &CacheController{
		cfg:     cfg,
		ctx:     ctx,
		cache:   cache,
		backend: backend,
	}
	c.runLogger(ctx)
	return c
}

// Index is the main HTTP handler for /api/v1/cache.
func (c *CacheController) Index(r *fasthttp.RequestCtx) {
	var from = time.Now()

	// Extract application context from request, fallback to base context.
	ctx, cancel := context.WithCancel(c.ctx)
	defer cancel()

	// Parse request parameters.
	req := model.NewRequestFromFasthttp(c.cfg.Cache, r)

	// Try to get response from cache.
	resp, found := c.cache.Get(req)
	defer resp.Close()
	if !found {
		var err error
		// On cache miss, get data from upstream backend and save in cache.
		resp, err = c.backend.Fetch(ctx, req)
		if err != nil {
			c.respondThatServiceIsTemporaryUnavailable(err, r)
			return
		}
		defer resp.Close()
		c.cache.Set(resp)
	}

	// Write status, headers, and body from the cached (or fetched) response.
	data := resp.Data()
	r.Response.SetStatusCode(data.StatusCode())
	for key, vv := range data.Headers() {
		for _, value := range vv {
			r.Response.Header.Add(key, value)
		}
	}
	// Add revalidation time as Last-Modified
	r.Response.Header.Add("Last-Modified", resp.RevalidatedAt().Format(http.TimeFormat))

	if _, err := serverutils.Write(data.Body(), r); err != nil {
		c.respondThatServiceIsTemporaryUnavailable(err, r)
		return
	}

	// Record the duration in debug mode for metrics.
	count.Add(1)
	duration.Add(time.Since(from).Nanoseconds())

	// Achieving the best performance. Increases CPU usage.
	runtime.Gosched()
}

// respondThatServiceIsTemporaryUnavailable returns 503 and logs the error.
func (c *CacheController) respondThatServiceIsTemporaryUnavailable(err error, ctx *fasthttp.RequestCtx) {
	log.Error().Err(err).Msg("[cache-controller] handle request error: " + err.Error()) // Don't move it down due to error will be rewritten.

	ctx.SetStatusCode(fasthttp.StatusServiceUnavailable)
	if _, err = serverutils.Write(c.resolveMessagePlaceholder(serviceUnavailableResponseBytes, err), ctx); err != nil {
		log.Err(err).Msg("failed to write into *fasthttp.RequestCtx")
	}
}

// respondThatTheRequestIsBad returns 400 and logs the error.
func (c *CacheController) respondThatTheRequestIsBad(err error, ctx *fasthttp.RequestCtx) {
	log.Err(err).Msg("[cache-controller] bad request: " + err.Error()) // Don't move it down due to error will be rewritten.

	ctx.SetStatusCode(fasthttp.StatusBadRequest)
	if _, err = serverutils.Write(c.resolveMessagePlaceholder(badRequestResponseBytes, err), ctx); err != nil {
		log.Err(err).Msg("failed to write into *fasthttp.RequestCtx")
	}
}

// resolveMessagePlaceholder substitutes ${message} in template with escaped error message.
func (c *CacheController) resolveMessagePlaceholder(msg []byte, err error) []byte {
	escaped, _ := json.Marshal(err.Error())
	return bytes.ReplaceAll(msg, messagePlaceholder, escaped[1:len(escaped)-1])
}

// AddRoute attaches controller's route(s) to the provided router.
func (c *CacheController) AddRoute(router *router.Router) {
	router.GET(CacheGetPath, c.Index)
}

// stat is an internal structure for windowed request statistics (for debug logging).
type stat struct {
	label    string
	divider  int // window size in seconds
	tickerCh <-chan time.Time
	count    int
	total    time.Duration
}

// runLogger runs a goroutine to periodically log RPS and avg duration per window, if debug enabled.
func (c *CacheController) runLogger(ctx context.Context) {
	go func() {
		t := utils.NewTicker(ctx, time.Second*5)
		for {
			select {
			case <-ctx.Done():
				return
			case <-t:
				c.logAndReset()
			default:
				runtime.Gosched()
			}
		}
	}()
}

// logAndReset prints and resets stat counters for a given window (5s, 1m, etc).
func (c *CacheController) logAndReset() {
	const secs int64 = 5

	var (
		avg string
		cnt = count.Load()
		dur = time.Duration(duration.Load())
		rps = strconv.Itoa(int(cnt / secs))
	)

	if cnt > 0 {
		avg = (dur / time.Duration(cnt)).String()
	} else {
		avg = zeroLiteral
	}

	log.
		Info().
		//Str("target", "cache-controller").
		//Str("period", s.label).
		//Str("rps", rps).
		//Str("avgDuration", avg).
		Msgf("[cache-controller][5s] served %d requests (rps: %s, avgDuration: %s)", cnt, rps, avg)

	count.Store(0)
	duration.Store(0)
}
