package repository

import (
	"bytes"
	"context"
	"errors"
	"github.com/Borislavv/advanced-cache/pkg/config"
	"github.com/Borislavv/advanced-cache/pkg/model"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
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
	Fetch(ctx context.Context, req *model.Request) (*model.Response, error)
	RevalidatorMaker(req *model.Request) func(ctx context.Context) (*model.Data, error)
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

// Fetch method fetches page data for the given request and constructs a cacheable response.
// It also attaches a revalidator closure for future background refreshes.
func (s *Backend) Fetch(ctx context.Context, req *model.Request) (*model.Response, error) {
	// Fetch data from backend.
	data, err := s.requestExternalBackend(ctx, req)
	if err != nil {
		return nil, errors.New("failed to request external backend: " + err.Error())
	}

	// Build a new response object, which contains the cache payload, request, config and revalidator.
	resp, err := model.NewResponse(data, req, s.cfg, s.RevalidatorMaker(req))
	if err != nil {
		return nil, errors.New("failed to create response: " + err.Error())
	}

	return resp, nil
}

// RevalidatorMaker builds a new revalidator for model.Response by catching a request into closure for be able to call backend later.
func (s *Backend) RevalidatorMaker(req *model.Request) func(ctx context.Context) (*model.Data, error) {
	return func(ctx context.Context) (*model.Data, error) {
		return s.requestExternalBackend(ctx, req)
	}
}

// requestExternalBackend actually performs the HTTP request to backend and parses the response.
// Returns a Data object suitable for caching.
func (s *Backend) requestExternalBackend(ctx context.Context, req *model.Request) (*model.Data, error) {
	// Apply a hard timeout for the HTTP request.
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Cache.Upstream.Timeout)
	defer cancel()

	url := s.cfg.Cache.Upstream.Url
	query := req.Query()

	// Efficiently concatenate base URL and query.
	queryBuf := make([]byte, 0, len(url)+len(req.Path())+len(query))
	queryBuf = append(queryBuf, url...)
	queryBuf = append(queryBuf, req.Path()...)
	queryBuf = append(queryBuf, query...)

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, string(queryBuf), nil)
	if err != nil {
		return nil, err
	}

	for _, hdr := range req.Headers() {
		request.Header.Add(string(hdr[0]), string(hdr[1]))
	}

	client := s.clientsPool.Get().(*http.Client)
	defer s.clientsPool.Put(client)

	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()

	// Read response body using a pooled reader to reduce allocations.
	var buf bytes.Buffer
	_, err = io.Copy(&buf, response.Body)
	if err != nil {
		return nil, err
	}

	return model.NewData(req.Rule(), response.StatusCode, response.Header, buf.Bytes()), nil
}
