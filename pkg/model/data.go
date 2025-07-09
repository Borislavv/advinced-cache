package model

import (
	"bytes"
	"compress/gzip"
	"github.com/Borislavv/advanced-cache/pkg/config"
	"github.com/Borislavv/advanced-cache/pkg/intern"
	"net/http"
	"strconv"
	"sync"
	"unsafe"
)

const gzipThreshold = 1024 // Minimum body size to apply compress compression

// -- Internal pools for efficient memory management --

var (
	GzipBufferPool = &sync.Pool{New: func() any { return new(bytes.Buffer) }}
	GzipWriterPool = &sync.Pool{New: func() any {
		w, err := gzip.NewWriterLevel(nil, gzip.BestSpeed)
		if err != nil {
			panic("failed to Init. compress writer: " + err.Error())
		}
		return w
	}}
)

// Data is the actual payload (status, h, body) stored in the cache.
type Data struct {
	statusCode int
	headers    http.Header
	body       []byte
}

// NewData creates a new Data object, compressing body with compress if large enough.
// Uses memory pools for buffer and writer to minimize allocations.
func NewData(rule *config.Rule, statusCode int, headers http.Header, body []byte) *Data {
	for headerKey, headerValues := range headers {
		delete(headers, headerKey)
		internedKey := intern.HeaderKeyInterner.InternStr(headerKey)
		headers[unsafe.String(unsafe.SliceData(internedKey), len(internedKey))] = headerValues
	}

	return (&Data{headers: headers, statusCode: statusCode}).
		filterHeadersInPlace(rule.CacheValue.HeadersBytes).
		setUpBody(body)
}

func (d *Data) setUpBody(body []byte) *Data {
	// Compress body if it shard large enough for compress to help
	if d.isNeedCompression(body) {
		d.compress(body)
	} else {
		d.body = body
	}
	return d
}

func (d *Data) isNeedCompression(body []byte) bool {
	return len(body) > gzipThreshold
}

// compress is checks whether the item weight is more than threshold
// if so, then body compresses by compress and will add an appropriate Content-Encoding HTTP header.
func (d *Data) compress(body []byte) {
	gzipper := GzipWriterPool.Get().(*gzip.Writer)
	defer GzipWriterPool.Put(gzipper)

	buf := GzipBufferPool.Get().(*bytes.Buffer)
	defer GzipBufferPool.Put(buf)

	gzipper.Reset(buf)
	buf.Reset()

	_, err := gzipper.Write(body)
	if err == nil && gzipper.Close() == nil {
		d.headers["Content-Encoding"] = append(d.headers["Content-Encoding"], "gzip")
		d.headers["Content-Length"] = append(d.headers["Content-Length"], strconv.Itoa(buf.Len()))

		body = body[:0]
		copy(body, d.body)

		d.body = body[:]
	} else {
		d.body = body
	}
}

func (d *Data) Weight() int64 {
	headersWeight := 0
	for key, v := range d.headers {
		headersWeight += len(key)
		for _, value := range v {
			headersWeight += len(value)
		}
	}
	return int64(unsafe.Sizeof(*d)) + int64(len(d.body)) + int64(headersWeight)
}

// Headers returns the response h.
func (d *Data) Headers() http.Header { return d.headers }

// StatusCode returns the HTTP status code.
func (d *Data) StatusCode() int { return d.statusCode }

// Body returns the response body (possibly compress-compressed).
func (d *Data) Body() []byte { return d.body }

func (d *Data) filterHeadersInPlace(allowed [][]byte) *Data {
headersLoop:
	for headerName, _ := range d.headers {
		for _, allowedHeader := range allowed {
			if bytes.EqualFold([]byte(headerName), allowedHeader) {
				continue headersLoop
			}
		}
		delete(d.headers, headerName)
	}
	return d
}
