package repository

import (
	"bytes"
	"github.com/Borislavv/advanced-cache/pkg/config"
	"github.com/Borislavv/advanced-cache/pkg/pools"
	"github.com/valyala/bytebufferpool"
	"github.com/valyala/fasthttp"
	"net"
	"net/http"
	"sync"
	"time"
	"unsafe"
)

var transport = &http.Transport{
	// Max idle (keep-alive) connections across ALL hosts
	MaxIdleConns: 10000,

	// Max idle (keep-alive) connections per host
	MaxIdleConnsPerHost: 1000,

	// Max concurrent connections per host (optional)
	MaxConnsPerHost: 0, // 0 = unlimited (use with caution)

	IdleConnTimeout: 30 * time.Second,

	// Optional: tune dialer
	DialContext: (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,

	// Optional: configure TLS handshake timeout, etc.
	TLSHandshakeTimeout: 10 * time.Second,

	// ExpectContinueTimeout: wait time for 100-continue
	ExpectContinueTimeout: 1 * time.Second,
}

// Backender defines the interface for a repository that provides SEO page data.
type Backender interface {
	Fetch(
		path []byte, query []byte, queryHeaders [][2][]byte,
	) (
		status int, headers [][2][]byte, body []byte, releaseFn func(), err error,
	)

	RevalidatorMaker() func(
		path []byte, query []byte, queryHeaders [][2][]byte,
	) (
		status int, headers [][2][]byte, body []byte, releaseFn func(), err error,
	)
}

// Backend implements the Backender interface.
// It fetches and constructs SEO page data responses from an external backend.
type Backend struct {
	cfg         *config.Cache // Global configuration (backend URL, etc)
	transport   *http.Transport
	clientsPool *sync.Pool
}

// NewBackend creates a new instance of Backend.
func NewBackend(cfg *config.Cache) *Backend {
	return &Backend{cfg: cfg, clientsPool: &sync.Pool{
		New: func() interface{} {
			return &http.Client{
				Transport: transport,
				Timeout:   10 * time.Second,
			}
		},
	}}
}

func (s *Backend) Fetch(
	path []byte, query []byte, queryHeaders [][2][]byte,
) (
	status int, headers [][2][]byte, body []byte, releaseFn func(), err error,
) {
	return s.requestExternalBackend(path, query, queryHeaders)
}

// RevalidatorMaker builds a new revalidator for model.Response by catching a request into closure for be able to call backend later.
func (s *Backend) RevalidatorMaker() func(
	path []byte, query []byte, queryHeaders [][2][]byte,
) (
	status int, headers [][2][]byte, body []byte, releaseFn func(), err error,
) {
	return func(
		path []byte, query []byte, queryHeaders [][2][]byte,
	) (
		status int, headers [][2][]byte, body []byte, releaseFn func(), err error,
	) {
		return s.requestExternalBackend(path, query, queryHeaders)
	}
}

var emptyReleaseFn = func() {}

// requestExternalBackend actually performs the HTTP request to backend and parses the response.
func (s *Backend) requestExternalBackend(
	path []byte, query []byte, queryHeaders [][2][]byte,
) (status int, headers [][2][]byte, body []byte, releaseFn func(), err error) {
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)

	req.Header.SetMethod(fasthttp.MethodGet)

	url := unsafe.Slice(unsafe.StringData(s.cfg.Cache.Upstream.Url), len(s.cfg.Cache.Upstream.Url))

	urlBuf := bytebufferpool.Get()
	defer func() {
		urlBuf.Reset()
		bytebufferpool.Put(urlBuf)
	}()
	if _, err = urlBuf.Write(url); err != nil {
		return 0, nil, nil, emptyReleaseFn, err
	}
	if _, err = urlBuf.Write(path); err != nil {
		return 0, nil, nil, emptyReleaseFn, err
	}
	if _, err = urlBuf.Write(query); err != nil {
		return 0, nil, nil, emptyReleaseFn, err
	}
	req.SetRequestURI(unsafe.String(unsafe.SliceData(urlBuf.Bytes()), urlBuf.Len()))

	for _, kv := range queryHeaders {
		req.Header.SetBytesKV(kv[0], kv[1])
	}

	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(resp)

	if err = pools.BackendHttpClientPool.DoTimeout(req, resp, s.cfg.Cache.Upstream.Timeout); err != nil {
		return 0, nil, nil, emptyReleaseFn, err
	}

	headers = pools.KeyValueSlicePool.Get().([][2][]byte)
	resp.Header.All()(func(k, v []byte) bool {
		keyCopied := pools.BackendBufPool.Get().([]byte)
		copy(keyCopied, k)
		valueCopied := pools.BackendBufPool.Get().([]byte)
		copy(valueCopied, v)
		headers = append(headers, [2][]byte{keyCopied, valueCopied})
		return true
	})

	buf := pools.BackendBodyBufferPool.Get().(*bytes.Buffer)
	if _, err = buf.Write(resp.Body()); err != nil {
		return 0, nil, nil, emptyReleaseFn, err
	}

	return resp.StatusCode(), headers, buf.Bytes(), func() {
		for _, kv := range headers {
			kv[0] = kv[0][:0]
			kv[1] = kv[1][:0]
			pools.BackendBufPool.Put(kv[0])
			pools.BackendBufPool.Put(kv[1])
		}
		headers = headers[:0]
		pools.KeyValueSlicePool.Put(headers)

		buf.Reset()
		pools.BackendBodyBufferPool.Put(buf)
	}, nil
}
