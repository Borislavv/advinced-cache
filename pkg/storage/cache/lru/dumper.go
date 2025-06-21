package lru

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"github.com/Borislavv/traefik-http-cache-plugin/pkg/model"
	sharded "github.com/Borislavv/traefik-http-cache-plugin/pkg/storage/map"
	"github.com/rs/zerolog/log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const dumpFileName = "cache.dump.gz"

var dumpEntryPool = sync.Pool{
	New: func() any { return new(dumpEntry) },
}

type dumpEntry struct {
	Unique     string      `json:"unique"`
	StatusCode int         `json:"statusCode"`
	Headers    http.Header `json:"headers"`
	Body       []byte      `json:"body"`
	Query      []byte      `json:"query"`
	Path       []byte      `json:"path"`
	MapKey     uint64      `json:"mapKey"`
	ShardKey   uint64      `json:"shardKey"`
}

func (c *Storage) DumpToDir(ctx context.Context, dir string) error {
	start := time.Now()
	filename := filepath.Join(dir, dumpFileName)

	f, err := os.Create(filename)
	if err != nil {
		return fmt.Errorf("create dump file error: %w", err)
	}
	defer func() { _ = f.Close() }()

	gz := gzip.NewWriter(f)
	defer func() { _ = gz.Close() }()

	bw := bufio.NewWriterSize(gz, 64*1024*1024) // 64MB
	defer func() { _ = bw.Flush() }()

	var success, errors int32
	enc := json.NewEncoder(bw)
	mu := sync.Mutex{}

	c.shardedMap.WalkShards(func(shardKey uint64, shard *sharded.Shard[*model.Response]) {
		shard.Walk(ctx, func(key uint64, resp *model.Response) bool {
			defer resp.Close()

			mu.Lock()
			defer mu.Unlock()

			e := dumpEntryPool.Get().(*dumpEntry)
			*e = dumpEntry{
				Unique:     fmt.Sprintf("%d-%d", shardKey, key),
				StatusCode: resp.Data().StatusCode(),
				Headers:    resp.Data().Headers(),
				Body:       resp.Data().Body(),
				Query:      resp.Request().ToQuery(),
				Path:       resp.Request().Path(),
				MapKey:     resp.Request().MapKey(),
				ShardKey:   resp.Request().ShardKey(),
			}

			if err = enc.Encode(e); err != nil {
				log.Err(err).Msg("[dump] entry encode error")
				atomic.AddInt32(&errors, 1)
			} else {
				atomic.AddInt32(&success, 1)
			}

			// Clean and put back to pool
			*e = dumpEntry{}
			dumpEntryPool.Put(e)
			return true
		}, true)
	})

	log.Info().Msgf("[dump] finished writing %d entries, errors: %d (elapsed: %s)", success, errors, time.Since(start))
	if errors > 0 {
		return fmt.Errorf("completed with %d errors", errors)
	}
	return nil
}

func (c *Storage) LoadFromDir(ctx context.Context, dir string) error {
	start := time.Now()
	filename := filepath.Join(dir, dumpFileName)

	f, err := os.Open(filename)
	if err != nil {
		return fmt.Errorf("open dump file error: %w", err)
	}
	defer func() { _ = f.Close() }()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip reader error: %w", err)
	}
	defer func() { _ = gz.Close() }()

	dec := json.NewDecoder(bufio.NewReaderSize(gz, 64*1024*1024)) // 64MB

	var success, failed int
	for dec.More() {
		if ctx.Err() != nil {
			log.Warn().Msg("[dump] context cancelled")
			return ctx.Err()
		}

		entry := dumpEntryPool.Get().(*dumpEntry)
		if err = dec.Decode(entry); err != nil {
			log.Err(err).Msg("[dump] decode error")
			failed++
			dumpEntryPool.Put(entry)
			continue
		}

		data := model.NewRawData(c.cfg, entry.Path, entry.StatusCode, entry.Headers, entry.Body)
		req := model.NewRawRequest(c.cfg, entry.MapKey, entry.ShardKey, entry.Query, entry.Path)
		resp, err := model.NewResponse(data, req, c.cfg, c.backend.RevalidatorMaker(req))
		if err != nil {
			log.Err(err).Msg("[dump] response build failed")
			failed++
			dumpEntryPool.Put(entry)
			continue
		}

		c.Set(resp)
		_ = resp.Close()
		dumpEntryPool.Put(entry)
		success++
	}

	log.Info().Msgf("[dump] restored %d entries, errors: %d (elapsed: %s)", success, failed, time.Since(start))
	if failed > 0 {
		return fmt.Errorf("load completed with %d errors", failed)
	}
	return nil
}
