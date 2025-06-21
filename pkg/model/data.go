package model

import (
	"bytes"
	"compress/gzip"
	"github.com/Borislavv/traefik-http-cache-plugin/pkg/config"
	"github.com/Borislavv/traefik-http-cache-plugin/pkg/resource"
	"github.com/Borislavv/traefik-http-cache-plugin/pkg/synced"
	"net/http"
	"sync"
	"unsafe"
)

var (
	dataPool = synced.NewBatchPool[*Data](func() *Data {
		return new(Data).clear()
	})
	GzipBufferPool = &sync.Pool{New: func() any { return new(bytes.Buffer) }}
	GzipWriterPool = &sync.Pool{New: func() any {
		w, err := gzip.NewWriterLevel(nil, gzip.BestSpeed)
		if err != nil {
			panic("failed to Init. gzip writer: " + err.Error())
		}
		return w
	}}
)

// Data is the actual payload (status, h, body) stored in the cache.
type Data struct {
	statusCode int
	headers    http.Header
	body       *resource.ReleasableBody
}

// NewData creates a new Data object, compressing body with gzip if large enough.
// Uses memory pools for buffer and writer to minimize allocations.
func NewData(cfg *config.Cache, path []byte, statusCode int, headers http.Header, body *resource.ReleasableBody) *Data {
	data := dataPool.Get()
	*data = Data{
		headers:    getAllowedValueHeaders(cfg, path, headers),
		statusCode: statusCode,
		body:       body,
	}

	// Compress body if it shard large enough for gzip to help
	if len(body.Bytes()) > gzipThreshold {
		gzipper := GzipWriterPool.Get().(*gzip.Writer)
		defer GzipWriterPool.Put(gzipper)

		buf := GzipBufferPool.Get().(*bytes.Buffer)
		defer GzipBufferPool.Put(buf)

		gzipper.Reset(buf)
		buf.Reset()

		_, err := gzipper.Write(body.Bytes())
		if err == nil && gzipper.Close() == nil {
			data.headers["Content-Encoding"] = append(data.headers["Content-Encoding"], "gzip")
			data.body.Reset()
			data.body.Append(buf.Bytes())
		}
	}

	return data
}

func NewRawData(cfg *config.Cache, path []byte, statusCode int, headers http.Header, body []byte) *Data {
	data := dataPool.Get()
	*data = Data{
		headers:    getAllowedValueHeaders(cfg, path, headers),
		statusCode: statusCode,
		body:       resource.AcquireBody().Append(body),
	}
	return data
}

func (d *Data) Weight() int64 {
	return int64(unsafe.Sizeof(*d)) + d.body.Weight()
}

// Headers returns the response h.
func (d *Data) Headers() http.Header { return d.headers }

// StatusCode returns the HTTP status code.
func (d *Data) StatusCode() int { return d.statusCode }

// Body returns the response body (possibly gzip-compressed).
func (d *Data) Body() []byte { return d.body.Bytes() }

// Release calls releaseFn and returns the Data to the pool.
func (d *Data) Release() {
	d.body.Release()
	d.clear()
	dataPool.Put(d)
}

// clear zeroes the Data fields before pooling.
func (d *Data) clear() *Data {
	d.statusCode = 0
	d.headers = nil
	d.body = nil
	return d
}

func filterValueHeadersInPlace(headers http.Header, allowed [][]byte) {
headersLoop:
	for headerName, _ := range headers {
		for _, allowedHeader := range allowed {
			if bytes.EqualFold([]byte(headerName), allowedHeader) {
				continue headersLoop
			}
		}
		delete(headers, headerName)
	}
}

func getValueAllowed(cfg *config.Cache, path []byte) (headers [][]byte) {
	headers = make([][]byte, 0, 7)
	for _, rule := range cfg.Cache.Rules {
		if bytes.HasPrefix(path, rule.PathBytes) {
			for _, header := range rule.CacheValue.HeadersBytes {
				headers = append(headers, header)
			}
		}
	}
	return headers
}

func getAllowedValueHeaders(cfg *config.Cache, path []byte, headers http.Header) http.Header {
	allowedValueHeaders := getValueAllowed(cfg, path)
	filterValueHeadersInPlace(headers, allowedValueHeaders)
	return headers
}
