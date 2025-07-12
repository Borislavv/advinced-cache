package advancedcache

import (
	"context"
	"github.com/Borislavv/advanced-cache/pkg/config"
	"github.com/Borislavv/advanced-cache/pkg/repository"
	"github.com/Borislavv/advanced-cache/pkg/storage"
	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/httpcaddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"net/http"
)

var _ caddy.Module = (*CacheMiddleware)(nil)

var (
	contentTypeKey                  = "Content-Type"
	applicationJsonValue            = "application/json"
	lastModifiedKey                 = "Last-Modified"
	serviceTemporaryUnavailableBody = []byte(`{"error":{"message":"Service temporarily unavailable."}}`)
)

const moduleName = "advanced_cache"

func init() {
	caddy.RegisterModule(&CacheMiddleware{})
	httpcaddyfile.RegisterHandlerDirective(moduleName, parseCaddyfile)
}

type CacheMiddleware struct {
	ConfigPath string
	ctx        context.Context
	cfg        *config.Cache
	store      storage.Storage
	backend    repository.Backender
	refresher  storage.Refresher
	evictor    storage.Evictor
	dumper     storage.Dumper
	count      int64 // Num
	duration   int64 // UnixNano
}

func (*CacheMiddleware) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.handlers." + moduleName,
		New: func() caddy.Module { return new(CacheMiddleware) },
	}
}

func (middleware *CacheMiddleware) Provision(ctx caddy.Context) error {
	return middleware.run(ctx.Context)
}

func (middleware *CacheMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request, next caddyhttp.Handler) error {
	//from := time.Now()

	// Build request (return error on rule missing for current path)
	//req, err := model.NewRequestFromNetHttp(middleware.cfg, r)
	//if err != nil {
	//	// If path does not match then process request manually without cache
	//	return next.ServeHTTP(w, r)
	//}

	//resp, isHit := middleware.store.Get(req)
	//if !isHit {
	//	captured := newCaptureRW()
	//
	//	// Handle request manually due to store it
	//	if srvErr := next.ServeHTTP(captured, r); srvErr != nil {
	//		captured.header = make(http.Header)
	//		captured.body.Reset()
	//		captured.status = http.StatusServiceUnavailable
	//		captured.WriteHeader(captured.status)
	//		_, _ = captured.Write(serviceTemporaryUnavailableBody)
	//	}
	//
	//	// Build new response
	//	data := model.NewData(req.Rule(), captured.status, captured.header, captured.body.Bytes())
	//	resp, _ = model.NewResponse(data, req, middleware.cfg, middleware.backend.RevalidatorMaker(req))
	//	middleware.store.Set(resp)
	//}
	//
	//// Apply custom http headers
	//for key, vv := range resp.Data().Headers() {
	//	for _, value := range vv {
	//		w.Header().Add(key, value)
	//	}
	//}
	//
	//// Apply standard http headers
	//w.Header().Add(contentTypeKey, applicationJsonValue)
	//w.Header().Add(lastModifiedKey, resp.RevalidatedAt().Format(http.TimeFormat))
	//w.WriteHeader(resp.Data().StatusCode())
	//
	//// Write response data
	//_, _ = w.Write(resp.Data().Body())

	//// Record the duration in debug mode for metrics.
	//atomic.AddInt64(&middleware.count, 1)
	//atomic.AddInt64(&middleware.duration, time.Since(from).Nanoseconds())
	//
	//return err
	return nil
}
