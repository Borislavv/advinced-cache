package advancedcache

import (
	"github.com/Borislavv/advanced-cache/pkg/model"
	"github.com/Borislavv/advanced-cache/pkg/repository"
	"github.com/Borislavv/advanced-cache/pkg/storage"
	"github.com/Borislavv/advanced-cache/pkg/storage/lfu"
	"github.com/Borislavv/advanced-cache/pkg/storage/lru"
	sharded "github.com/Borislavv/advanced-cache/pkg/storage/map"
)

func (middleware *CacheMiddleware) setUpCache() {
	shardedMap := sharded.NewMap[*model.Entry](middleware.ctx, middleware.cfg.Cache.Preallocate.PerShard)
	middleware.backend = repository.NewBackend(middleware.cfg)
	balancer := lru.NewBalancer(middleware.ctx, shardedMap)
	middleware.store = lru.NewStorage(middleware.ctx, middleware.cfg, balancer, middleware.backend, lfu.NewTinyLFU(middleware.ctx), shardedMap)
	middleware.refresher = storage.NewRefresher(middleware.ctx, middleware.cfg, balancer, middleware.store)
	middleware.dumper = storage.NewDumper(middleware.cfg, shardedMap, middleware.store, middleware.backend)
	middleware.evictor = storage.NewEvictor(middleware.ctx, middleware.cfg, middleware.store, balancer)
}
