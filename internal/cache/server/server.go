package server

import (
	"context"
	"errors"
	"github.com/Borislavv/advanced-cache/internal/cache/api"
	"github.com/Borislavv/advanced-cache/internal/cache/config"
	"github.com/Borislavv/advanced-cache/pkg/k8s/probe/liveness"
	"github.com/Borislavv/advanced-cache/pkg/prometheus/metrics"
	"github.com/Borislavv/advanced-cache/pkg/repository"
	httpserver "github.com/Borislavv/advanced-cache/pkg/server"
	"github.com/Borislavv/advanced-cache/pkg/server/controller"
	"github.com/Borislavv/advanced-cache/pkg/server/middleware"
	"github.com/Borislavv/advanced-cache/pkg/storage"
	"github.com/rs/zerolog/log"
	"sync"
	"sync/atomic"
)

var (
	InitFailedErrorMessage = "[server] init. failed"
)

// Http interface exposes methods for starting and liveness probing.
type Http interface {
	Start()
	IsAlive() bool
}

// HttpServer implements Http, wraps all dependencies required for running the HTTP server.
type HttpServer struct {
	ctx           context.Context
	cfg           *config.Config
	db            storage.Storage
	backend       repository.Backender
	probe         liveness.Prober
	metrics       metrics.Meter
	server        httpserver.Server
	isServerAlive *atomic.Bool
}

// New creates a new HttpServer, initializing metrics and the HTTP server itself.
// If any step fails, returns an error and performs cleanup.
func New(
	ctx context.Context,
	cfg *config.Config,
	db storage.Storage,
	backend repository.Backender,
	probe liveness.Prober,
	meter metrics.Meter,
) (*HttpServer, error) {
	var err error

	srv := &HttpServer{
		ctx:           ctx,
		cfg:           cfg,
		db:            db,
		backend:       backend,
		probe:         probe,
		metrics:       meter,
		isServerAlive: &atomic.Bool{},
	}

	// Initialize HTTP server with all controllers and middlewares.
	if err = srv.initServer(); err != nil {
		log.Err(err).Msg(InitFailedErrorMessage)
		return nil, errors.New(InitFailedErrorMessage)
	}

	return srv, nil
}

// Start runs the HTTP server in a goroutine and waits for it to finish.
func (s *HttpServer) Start() {
	waitCh := make(chan struct{})

	go func() {
		defer close(waitCh)
		wg := &sync.WaitGroup{}
		defer wg.Wait()
		s.spawnServer(wg)
	}()

	<-waitCh
}

// IsAlive returns true if the server is marked as alive.
func (s *HttpServer) IsAlive() bool {
	return s.isServerAlive.Load()
}

// spawnServer starts the HTTP server in a new goroutine, sets server liveness flags, and blocks until it exits.
func (s *HttpServer) spawnServer(wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer func() {
			s.isServerAlive.Store(false)
			wg.Done()
		}()
		s.isServerAlive.Store(true)
		s.server.ListenAndServe()
	}()
}

// initServer creates the HTTP server instance, sets up controllers and middlewares, and stores the result.
func (s *HttpServer) initServer() error {
	// Compose server with controllers and middlewares.
	if server, err := httpserver.New(s.ctx, s.cfg.Server, s.controllers(), s.middlewares()); err != nil {
		log.Err(err).Msg(InitFailedErrorMessage)
		return errors.New(InitFailedErrorMessage)
	} else {
		s.server = server
	}

	return nil
}

// controllers returns all HTTP controllers for the server (endpoints/handlers).
func (s *HttpServer) controllers() []controller.HttpController {
	return []controller.HttpController{
		api.NewCacheController(s.ctx, s.cfg, s.db, s.backend), // Main cache handler
		liveness.NewController(s.probe),                       // Liveness/healthcheck endpoint
		//api2.NewPrometheusMetrics(),                           // Metrics endpoint
	}
}

// middlewares returns the request middlewares for the server, executed in reverse order.
func (s *HttpServer) middlewares() []middleware.HttpMiddleware {
	return []middleware.HttpMiddleware{
		/** exec 1st. */ middleware.NewApplicationJsonMiddleware(), // Sets Content-Type
		///** exec 2rd. */ middleware2.NewPrometheusMetrics(s.ctx, s.metrics), // Metrics instrumentation
	}
}
