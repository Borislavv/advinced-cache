package cache

import (
	"context"
	"github.com/Borislavv/advanced-cache/internal/cache/config"
	"github.com/Borislavv/advanced-cache/internal/cache/server"
	"github.com/Borislavv/advanced-cache/pkg/gc"
	"github.com/Borislavv/advanced-cache/pkg/k8s/probe/liveness"
	"github.com/Borislavv/advanced-cache/pkg/model"
	"github.com/Borislavv/advanced-cache/pkg/prometheus/metrics"
	"github.com/Borislavv/advanced-cache/pkg/repository"
	"github.com/Borislavv/advanced-cache/pkg/shutdown"
	"github.com/Borislavv/advanced-cache/pkg/storage"
	"github.com/Borislavv/advanced-cache/pkg/storage/lfu"
	"github.com/Borislavv/advanced-cache/pkg/storage/lru"
	sharded "github.com/Borislavv/advanced-cache/pkg/storage/map"
	"github.com/Borislavv/advanced-cache/pkg/utils"
	"github.com/rs/zerolog/log"
	"time"
)

// App defines the cache application lifecycle interface.
type App interface {
	Start()
}

// Cache encapsulates the entire cache application state, including HTTP server, config, and probes.
type Cache struct {
	cfg        *config.Config
	ctx        context.Context
	cancel     context.CancelFunc
	probe      liveness.Prober
	server     server.Http
	metrics    metrics.Meter
	db         storage.Storage
	dumper     storage.Dumper
	balancer   lru.Balancer
	evictor    storage.Evictor
	refresher  storage.Refresher
	backend    repository.Backender
	shardedMap *sharded.Map[*model.Entry]
}

// NewApp builds a new Cache app, wiring together db, repo, reader, and server.
func NewApp(ctx context.Context, cfg *config.Config, probe liveness.Prober) (*Cache, error) {
	ctx, cancel := context.WithCancel(ctx)

	// Setup sharded map for high-concurrency cache db.
	shardedMap := sharded.NewMap[*model.Entry](ctx, cfg.Cache.Cache.Preallocate.PerShard)
	backend := repository.NewBackend(cfg.Cache)
	balancer := lru.NewBalancer(ctx, shardedMap)
	tinyLFU := lfu.NewTinyLFU(ctx)
	db := lru.NewStorage(ctx, cfg.Cache, balancer, backend, tinyLFU, shardedMap)
	refresher := storage.NewRefresher(ctx, cfg.Cache, balancer, db)
	dumper := storage.NewDumper(cfg.Cache, shardedMap, db, backend)
	evictor := storage.NewEvictor(ctx, cfg.Cache, db, balancer)
	meter := metrics.New()

	c := &Cache{
		ctx:        ctx,
		cancel:     cancel,
		cfg:        cfg,
		probe:      probe,
		db:         db,
		dumper:     dumper,
		evictor:    evictor,
		metrics:    meter,
		backend:    backend,
		balancer:   balancer,
		refresher:  refresher,
		shardedMap: shardedMap,
	}

	// Compose the HTTP server (API, metrics and so on)
	srv, err := server.New(ctx, cfg, db, backend, probe, meter)
	if err != nil {
		cancel()
		return nil, err
	}
	c.server = srv

	return c.run(), nil
}

// Start runs the cache server and liveness probe, and handles graceful shutdown.
// The Gracefuller interface is expected to call Done() when shutdown is complete.
func (c *Cache) Start(gc shutdown.Gracefuller) {
	defer func() {
		c.stop()
		gc.Done()
	}()

	log.Info().Msg("[app] starting cache")

	waitCh := make(chan struct{})

	go func() {
		defer close(waitCh)
		c.probe.Watch(c) // Call first due to it does not block the green-thread
		c.server.Start() // Blocks the green-thread until the server will be stopped
	}()

	log.Info().Msg("[app] cache has been started")

	<-waitCh // Wait until the server exits
}

func (c *Cache) runMetricsWriter() {
	go func() {
		t := utils.NewTicker(c.ctx, time.Second)
		for {
			select {
			case <-c.ctx.Done():
				return
			case <-t:
				memUsage, length := c.db.Stat()
				c.metrics.SetCacheLength(length)
				c.metrics.SetCacheMemory(memUsage)
			}
		}
	}()
}

func (c *Cache) run() *Cache {
	if err := c.dumper.Load(c.ctx); err != nil {
		log.Warn().Msg("[dump] failed to load dump: " + err.Error())
	}

	c.db.Run()
	c.evictor.Run()
	c.refresher.Run()
	c.runMetricsWriter()
	gc.Run(c.ctx, c.cfg.Cache)

	return c
}

// stop cancels the main application context and logs shutdown.
func (c *Cache) stop() {
	log.Info().Msg("[app] stopping cache")
	defer c.cancel()

	// spawn a new one with limit for k8s timeout before the service will be received SIGKILL
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	if err := c.dumper.Dump(ctx); err != nil {
		log.Err(err).Msg("[dump] failed to dump cache")
	}

	log.Info().Msg("[app] cache has been stopped")
}

// IsAlive is called by liveness probes to check app health.
// Returns false if the HTTP server is not alive.
func (c *Cache) IsAlive(_ context.Context) bool {
	if !c.server.IsAlive() {
		log.Info().Msg("[app] http server has gone away")
		return false
	}
	return true
}
