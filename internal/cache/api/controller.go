package api

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/Borislavv/advanced-cache/internal/cache/config"
	"github.com/Borislavv/advanced-cache/pkg/model"
	"github.com/Borislavv/advanced-cache/pkg/pools"
	"github.com/Borislavv/advanced-cache/pkg/repository"
	serverutils "github.com/Borislavv/advanced-cache/pkg/server/utils"
	"github.com/Borislavv/advanced-cache/pkg/storage"
	"github.com/Borislavv/advanced-cache/pkg/utils"
	"github.com/fasthttp/router"
	"github.com/rs/zerolog/log"
	"github.com/valyala/fasthttp"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

// CacheGetPath for getting pagedata from cache via HTTP.
const CacheGetPath = "/{any:*}"

// Predefined HTTP response templates for error handling (400/503)
var (
	serviceUnavailableResponseBytes = []byte(`{
	  "status": 503,
	  "error": "Service Unavailable",
	  "message": "` + string(messagePlaceholder) + `"
	}`)
	messagePlaceholder = []byte("${message}")
	hdrLastModified    = []byte("Last-Modified")
)

// Buffered channel for request durations (used only if debug enabled)
var (
	count    = &atomic.Int64{} // Num
	duration = &atomic.Int64{} // UnixNano
	fnd      = &atomic.Int64{}
	notFnd   = &atomic.Int64{}
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

func (c *CacheController) queryHeaders(r *fasthttp.RequestCtx) (headers [][2][]byte, releaseFn func()) {
	headers = pools.KeyValueSlicePool.Get().([][2][]byte)
	r.Request.Header.All()(func(key []byte, value []byte) bool {
		headers = append(headers, [2][]byte{key, value})
		return true
	})
	return headers, func() {
		headers = headers[:0]
		pools.KeyValueSlicePool.Put(headers)
	}
}

// Index is the main HTTP handler.
func (c *CacheController) Index(r *fasthttp.RequestCtx) {
	var from = time.Now()

	entry, entryReleaser, err := model.NewEntry(c.cfg.Cache, r)
	if err != nil {
		c.respondThatServiceIsTemporaryUnavailable(err, r)
		return
	}

	var (
		status   int
		headers  [][2][]byte
		body     []byte
		releaser func()
	)

	value, found := c.cache.Get(entry)
	if !found {
		notFnd.Add(1)

		path := r.Path()
		queryString := r.QueryArgs().QueryString()
		queryHeaders, queryReleaser := c.queryHeaders(r)
		defer queryReleaser()

		status, headers, body, releaser, err = c.backend.Fetch(path, queryString, queryHeaders)
		defer releaser()
		if err != nil {
			c.respondThatServiceIsTemporaryUnavailable(err, r)
			return
		}
		entry.SetPayload(path, queryString, queryHeaders, headers, body, status)

		entry.SetRevalidator(c.backend.RevalidatorMaker())
		c.cache.Set(entry)
		value = entry
	} else {
		fnd.Add(1)
		defer entryReleaser() // release the entry only on the case when existing entry was found in cache,
		// otherwise created entry will escape to heap (link must be alive while entry in cache)

		_, _, _, headers, body, status, releaser, err = value.Payload()
		defer releaser()
		if err != nil {
			c.respondThatServiceIsTemporaryUnavailable(err, r)
			return
		}
	}

	// Write status, headers, and body from the cached (or fetched) response.
	r.Response.SetStatusCode(status)
	for _, kv := range headers {
		r.Response.Header.AddBytesKV(kv[0], kv[1])
	}

	// Set up Last-Modified header
	c.setLastModifiedHeader(r, value, status)

	// Write body
	if _, err = serverutils.Write(body, r); err != nil {
		c.respondThatServiceIsTemporaryUnavailable(err, r)
		return
	}

	// Record the duration in debug mode for metrics.
	count.Add(1)
	duration.Add(time.Since(from).Nanoseconds())
}

// respondThatServiceIsTemporaryUnavailable returns 503 and logs the error.
func (c *CacheController) respondThatServiceIsTemporaryUnavailable(err error, ctx *fasthttp.RequestCtx) {
	log.Error().Err(err).Msg("[cache-controller] handle request error: " + err.Error()) // Don't move it down due to error will be rewritten.

	ctx.SetStatusCode(fasthttp.StatusServiceUnavailable)
	if _, err = serverutils.Write(c.resolveMessagePlaceholder(serviceUnavailableResponseBytes, err), ctx); err != nil {
		log.Err(err).Msg("failed to write into *fasthttp.RequestCtx")
	}
}

func (c *CacheController) setLastModifiedHeader(r *fasthttp.RequestCtx, entry *model.Entry, status int) {
	if status == http.StatusOK {
		r.Request.Header.SetBytesKV(
			hdrLastModified,
			time.Unix(0, entry.WillUpdateAt()-entry.Rule().TTL.Nanoseconds()).AppendFormat(nil, http.TimeFormat),
		)
	} else {
		r.Request.Header.SetBytesKV(
			hdrLastModified,
			time.Unix(0, entry.WillUpdateAt()-entry.Rule().ErrorTTL.Nanoseconds()).AppendFormat(nil, http.TimeFormat),
		)
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
			}
		}
	}()
}

// logAndReset prints and resets stat counters for a given window (5s).
func (c *CacheController) logAndReset() {
	const secs int64 = 5

	var (
		avg string
		cnt = count.Load()
		dur = time.Duration(duration.Load())
		rps = strconv.Itoa(int(cnt / secs))
	)

	if cnt <= 0 {
		return
	}

	avg = (dur / time.Duration(cnt)).String()

	logEvent := log.Info()

	if c.cfg.IsProd() {
		logEvent.
			Str("target", "controller").
			Str("rps", rps).
			Str("served", strconv.Itoa(int(cnt))).
			Str("periodMs", "5000").
			Str("avgDuration", avg)
	}

	logEvent.Msgf("[controller][5s] served %d requests (rps: %s, avgDuration: %s), hits: %d, misses: %d,", cnt, rps, avg, fnd.Load(), notFnd.Load())

	count.Store(0)
	duration.Store(0)
}
