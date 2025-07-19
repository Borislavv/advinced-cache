package middleware

import (
	"context"
	"github.com/Borislavv/advanced-cache/pkg/config"
	"github.com/Borislavv/advanced-cache/pkg/header"
	"github.com/Borislavv/advanced-cache/pkg/model"
	"github.com/Borislavv/advanced-cache/pkg/repository"
	"github.com/Borislavv/advanced-cache/pkg/storage"
	httpwriter "github.com/Borislavv/advanced-cache/pkg/writer"
	"net/http"
	"sync/atomic"
	"time"
	"unsafe"
)

var (
	contentTypeKey       = "Content-Type"
	applicationJsonValue = "application/json"
)

type TraefikCacheMiddleware struct {
	ctx       context.Context
	next      http.Handler
	name      string
	cfg       *config.Cache
	storage   storage.Storage
	backend   repository.Backender
	refresher storage.Refresher
	evictor   storage.Evictor
	dumper    storage.Dumper
	count     int64 // Num
	duration  int64 // UnixNano
}

func New(ctx context.Context, next http.Handler, name string) http.Handler {
	cacheMiddleware := &TraefikCacheMiddleware{
		ctx:  ctx,
		next: next,
		name: name,
	}

	if err := cacheMiddleware.run(ctx); err != nil {
		panic(err)
	}

	return cacheMiddleware
}

func (m *TraefikCacheMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	from := time.Now()

	newEntry, err := model.NewEntryNetHttp(m.cfg, r)
	if err != nil {
		// Path was not matched, then handle request through upstream without cache.
		m.next.ServeHTTP(w, r)
		return
	}

	var (
		status  int
		headers [][2][]byte
		body    []byte
	)

	foundEntry, found := m.storage.Get(newEntry)
	if !found {
		// MISS — prepare capture writer
		captured, releaseCapturer := httpwriter.NewCaptureResponseWriter(w)
		defer releaseCapturer()

		// Run downstream handler
		m.next.ServeHTTP(captured, r)

		// path is immutable and used only inside request
		path := unsafe.Slice(unsafe.StringData(r.URL.Path), len(r.URL.Path))

		// query immutable and used only inside request
		query := unsafe.Slice(unsafe.StringData(r.URL.RawQuery), len(r.URL.RawQuery))

		// Get query headers from original request
		queryHeaders, queryHeadersReleaser := newEntry.GetFilteredAndSortedKeyHeadersNetHttp(r)
		defer queryHeadersReleaser()

		var extractReleaser func()
		status, headers, body, extractReleaser = captured.ExtractPayload()
		defer extractReleaser()

		// Save the response into the new newEntry
		newEntry.SetPayload(path, query, queryHeaders, headers, body, status)
		newEntry.SetRevalidator(m.backend.RevalidatorMaker())

		// build and storage new Entry in cache
		foundEntry = m.storage.Set(model.NewVersionPointer(newEntry))
		defer foundEntry.Release() // an Entry stored in the cache must be released after use
	} else {
		// deferred release and remove
		newEntry.Remove()          // new Entry which was used as request for query cache does not need anymore
		defer foundEntry.Release() // an Entry retrieved from the cache must be released after use

		// Always read from cached foundEntry
		var queryHeaders [][2][]byte
		_, _, queryHeaders, headers, body, status, err = foundEntry.Payload()
		defer foundEntry.ReleasePayload(queryHeaders, headers)
		if err != nil {
			m.next.ServeHTTP(w, r)

			// Error — prepare capture writer
			captured, releaseCapturer := httpwriter.NewCaptureResponseWriter(w)
			defer releaseCapturer()

			var extractReleaser func()
			status, headers, body, extractReleaser = captured.ExtractPayload()
			defer extractReleaser()
		}
	}

	// Write cached headers
	for _, kv := range headers {
		w.Header().Add(
			unsafe.String(unsafe.SliceData(kv[0]), len(kv[0])),
			unsafe.String(unsafe.SliceData(kv[1]), len(kv[1])),
		)
	}

	// Last-Modified
	header.SetLastModified(w, foundEntry, status)

	// Content-Type
	w.Header().Set(contentTypeKey, applicationJsonValue)

	// StatusCode-code
	w.WriteHeader(status)

	// Write a response body
	_, _ = w.Write(body)

	// Metrics
	atomic.AddInt64(&m.count, 1)
	atomic.AddInt64(&m.duration, time.Since(from).Nanoseconds())
}
