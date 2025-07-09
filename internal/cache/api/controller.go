package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/Borislavv/advanced-cache/internal/cache/config"
	"github.com/Borislavv/advanced-cache/pkg/intern"
	"github.com/Borislavv/advanced-cache/pkg/mock"
	"github.com/Borislavv/advanced-cache/pkg/model"
	"github.com/Borislavv/advanced-cache/pkg/repository"
	"github.com/Borislavv/advanced-cache/pkg/rules"
	serverutils "github.com/Borislavv/advanced-cache/pkg/server/utils"
	"github.com/Borislavv/advanced-cache/pkg/storage"
	"github.com/Borislavv/advanced-cache/pkg/utils"
	"github.com/fasthttp/router"
	"github.com/rs/zerolog/log"
	"github.com/valyala/fasthttp"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// CacheGetPath for getting pagedata from cache via HTTP.
const CacheGetPath = "/{any:*}"

// Predefined HTTP response templates for error handling (400/503)
var (
	ruleNotFoundError               = errors.New("rule not found")
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
	go func() {
		path := []byte("/api/v2/pagedata")
		for resp := range mock.StreamRandomResponses(ctx, c.cfg.Cache, path, 5_000_000) {
			c.cache.Set(resp)
		}
	}()
	c.runLogger(ctx)
	return c
}

// Index is the main HTTP handler for /api/v1/cache.
func (c *CacheController) Index(r *fasthttp.RequestCtx) {
	var from = time.Now()

	path := intern.PathInterner.Intern(r.Path())

	rule := rules.Match(c.cfg.Cache, path)
	if rule == nil {
		c.respondThatServiceIsTemporaryUnavailable(ruleNotFoundError, r)
		return
	}

	queries := getFilteredAndSortedKeyQueriesFastHttp(r, rule.CacheKey.QueryBytes)
	headers := getFilteredAndSortedKeyHeadersFastHttp(&r.Request.Header, rule.CacheKey.HeadersBytes)
	defer func() {
		queries = queries[:0]
		queriesPool.Put(queries)
		headers = headers[:0]
		headersPool.Put(headers)
	}()

	req := model.NewRequest(rule, path, queries, headers)

	// Try to get response from cache.
	resp, found := c.cache.Get(req)
	if !found {
		req.SetUpQueryAndHeaders(queries, headers)
		// On cache miss, get data from upstream backend and save in cache.
		computed, err := c.backend.Fetch(c.ctx, req)
		if err != nil {
			c.respondThatServiceIsTemporaryUnavailable(err, r)
			return
		}
		resp = computed
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

	// Set up Last-Modified header
	r.Response.Header.SetBytesKV(hdrLastModified, resp.RevalidatedAt().AppendFormat(nil, http.TimeFormat))
	if _, err := serverutils.Write(data.Body(), r); err != nil {
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

	logEvent.Msgf("[controller][5s] served %d requests (rps: %s, avgDuration: %s)", cnt, rps, avg)

	count.Store(0)
	duration.Store(0)
}

var (
	queriesPool = sync.Pool{
		New: func() any { return make([][2][]byte, 0, 10) },
	}
	headersPool = sync.Pool{
		New: func() any { return make([][2][]byte, 0, 8) },
	}
)

func getFilteredAndSortedKeyQueriesFastHttp(ctx *fasthttp.RequestCtx, allowed [][]byte) [][2][]byte {
	if len(allowed) == 0 {
		return nil
	}

	filtered := queriesPool.Get().([][2][]byte)
	filtered = filtered[:0] // reuse but zero length

	ctx.QueryArgs().All()(func(k, v []byte) bool {
		for _, ak := range allowed {
			if bytes.HasPrefix(k, ak) {
				internedKey := intern.QueryKeyInterner.Intern(k)
				// NOTE: safe copy for value
				filtered = append(filtered, [2][]byte{internedKey, append([]byte(nil), v...)})
				break
			}
		}
		return true
	})

	// Sort in place
	sort.Slice(filtered, func(i, j int) bool {
		return bytes.Compare(filtered[i][0], filtered[j][0]) < 0
	})

	return filtered
}

func getFilteredAndSortedKeyHeadersFastHttp(r *fasthttp.RequestHeader, allowed [][]byte) [][2][]byte {
	if len(allowed) == 0 {
		return nil
	}

	filtered := headersPool.Get().([][2][]byte)
	filtered = filtered[:0] // reuse but zero length

	r.All()(func(k, v []byte) bool {
		for _, ak := range allowed {
			if bytes.EqualFold(k, ak) {
				internedKey := intern.HeaderKeyInterner.Intern(k)
				// NOTE: safe copy for value
				filtered = append(filtered, [2][]byte{internedKey, append([]byte(nil), v...)})
				break
			}
		}
		return true
	})

	// Sort in place
	sort.Slice(filtered, func(i, j int) bool {
		return bytes.Compare(filtered[i][0], filtered[j][0]) < 0
	})

	return filtered
}
