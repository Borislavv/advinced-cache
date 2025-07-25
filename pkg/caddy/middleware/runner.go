package advancedcache

import (
	"context"
	"github.com/Borislavv/advanced-cache/pkg/gc"
	"github.com/rs/zerolog/log"
)

func (m *CacheMiddleware) run(ctx context.Context) error {
	log.Info().Msg("[advanced-cache] starting")

	m.ctx = ctx

	if err := m.loadConfig(); err != nil {
		return err
	}

	enabled.Store(m.cfg.Cache.Enabled)

	m.setUpCache()

	if err := m.loadDump(); err != nil {
		log.Error().Err(err).Msg("[dump] failed to load")
	}

	m.storage.Run()
	m.evictor.run()
	m.refresher.run()
	m.runLoggerMetricsWriter()
	gc.Run(ctx, m.cfg)

	log.Info().Msg("[advanced-cache] has been started")

	return nil
}
