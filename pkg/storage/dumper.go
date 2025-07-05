package storage

import (
	"bufio"
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"github.com/Borislavv/advanced-cache/pkg/config"
	"github.com/Borislavv/advanced-cache/pkg/repository"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Borislavv/advanced-cache/pkg/model"
	sharded "github.com/Borislavv/advanced-cache/pkg/storage/map"
	"github.com/rs/zerolog/log"
)

var dumpEntryPool = sync.Pool{
	New: func() any { return new(dumpEntry) },
}

var dumpIsNotEnabledErr = errors.New("persistence mode is not enabled")

type dumpEntry struct {
	Unique          string      `json:"unique"`
	StatusCode      int         `json:"statusCode"`
	ResponseHeaders http.Header `json:"responseHeaders"`
	Body            []byte      `json:"body"`
	Query           [][2][]byte `json:"query"`
	QueryHeaders    [][2][]byte `json:"queryHeaders"`
	Path            []byte      `json:"path"`
	MapKey          uint64      `json:"mapKey"`
	ShardKey        uint64      `json:"shardKey"`
}

type Dumper interface {
	Dump(ctx context.Context) error
	Load(ctx context.Context) error
}

type Dump struct {
	cfg        *config.Cache
	shardedMap *sharded.Map[*model.Response] // Sharded storage for cache entries
	storage    Storage
	backend    repository.Backender
}

func NewDumper(
	cfg *config.Cache,
	shardedMap *sharded.Map[*model.Response],
	storage Storage,
	backend repository.Backender,
) *Dump {
	return &Dump{
		cfg:        cfg,
		shardedMap: shardedMap,
		storage:    storage,
		backend:    backend,
	}
}

// Dump writes cache entries to disk based on the configured format and rotation policy.
func (d *Dump) Dump(ctx context.Context) error {
	cfg := d.cfg.Cache.Persistence.Dump
	if !cfg.IsEnabled {
		return dumpIsNotEnabledErr
	}
	start := time.Now()

	// Ensure dump dir exists
	if err := os.MkdirAll(cfg.Dir, 0755); err != nil {
		return fmt.Errorf("create dump dir: %w", err)
	}

	// Use one common timestamp for all shard files
	timestamp := time.Now().Format("20060102T150405")

	// Parallel dump: each shard → separate file
	var wg sync.WaitGroup
	errCh := make(chan error, sharded.NumOfShards)

	var successNum, errorNum int32

	d.shardedMap.WalkShards(func(shardKey uint64, shard *sharded.Shard[*model.Response]) {
		wg.Add(1)
		go func(shardKey uint64, shard *sharded.Shard[*model.Response]) {
			defer wg.Done()

			filename := fmt.Sprintf("%s/%s-shard-%d-%s.dump", cfg.Dir, cfg.Name, shardKey, timestamp)
			tmpName := filename + ".tmp"

			f, err := os.Create(tmpName)
			if err != nil {
				errCh <- fmt.Errorf("create dump temp file: %w", err)
				return
			}
			defer func() { _ = f.Close() }()

			bw := bufio.NewWriter(f)
			enc := gob.NewEncoder(bw)

			shard.Walk(ctx, func(key uint64, resp *model.Response) bool {
				e := dumpEntryPool.Get().(*dumpEntry)
				*e = dumpEntry{
					Unique:          fmt.Sprintf("%d-%d", shardKey, key),
					StatusCode:      resp.Data().StatusCode(),
					ResponseHeaders: resp.Data().Headers(),
					Body:            resp.Data().Body(),
					Query:           resp.Request().QueryKV(),
					QueryHeaders:    resp.Request().Headers(),
					Path:            resp.Request().Path(),
					MapKey:          resp.Request().MapKey(),
					ShardKey:        resp.Request().ShardKey(),
				}

				if err = enc.Encode(e); err != nil {
					log.Error().Err(err).Msg("[dump] entry encode error")
					atomic.AddInt32(&errorNum, 1)
					errCh <- err
				} else {
					atomic.AddInt32(&successNum, 1)
				}

				*e = dumpEntry{}
				dumpEntryPool.Put(e)
				return true
			}, true)

			if err = bw.Flush(); err != nil {
				errCh <- fmt.Errorf("flush error: %w", err)
			}
			if err = os.Rename(tmpName, filename); err != nil {
				errCh <- fmt.Errorf("rename dump file: %w", err)
			}
		}(shardKey, shard)
	})

	wg.Wait()
	close(errCh)

	for err := range errCh {
		if err != nil {
			return err
		}
	}

	// Rotate old groups AFTER a successful dump
	if cfg.RotatePolicy == "ring" {
		if err := rotateOldFiles(cfg.Dir, cfg.Name, ".dump", cfg.MaxFiles); err != nil {
			log.Error().Err(err).Msg("[dump] rotation error")
		}
	}

	log.Info().Msgf("[dump] finished writing %d entries, errors: %d (elapsed: %s)", successNum, errorNum, time.Since(start))
	if errorNum > 0 {
		return fmt.Errorf("dump completed with %d errors", errorNum)
	}
	return nil
}

// Load restores cache entries from disk based on configuration.
func (d *Dump) Load(ctx context.Context) error {
	cfg := d.cfg.Cache.Persistence.Dump
	if !cfg.IsEnabled {
		return dumpIsNotEnabledErr
	}
	start := time.Now()

	// Find all dump shards
	files, err := filepath.Glob(fmt.Sprintf("%s/%s-shard-*.dump", cfg.Dir, cfg.Name))
	if err != nil {
		return fmt.Errorf("glob dump files: %w", err)
	}
	if len(files) == 0 {
		return fmt.Errorf("no dump files found in %s", cfg.Dir)
	}

	// Extract the latest timestamp and filter files
	latestTs := extractLatestTimestamp(files)
	filesToLoad := filterFilesByTimestamp(files, latestTs)
	if len(filesToLoad) == 0 {
		return fmt.Errorf("no dump files found for latest timestamp %s", latestTs)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, len(filesToLoad))
	var successNum, errorNum int32

	for _, file := range filesToLoad {
		wg.Add(1)
		go func(file string) {
			defer wg.Done()

			f, ferr := os.Open(file)
			if ferr != nil {
				errCh <- fmt.Errorf("open dump file: %w", ferr)
				return
			}
			defer func() { _ = f.Close() }()

			dec := gob.NewDecoder(bufio.NewReader(f))
		loop:
			for {
				select {
				case <-ctx.Done():
					return
				default:
					entry := dumpEntryPool.Get().(*dumpEntry)
					if err = dec.Decode(entry); err == io.EOF {
						dumpEntryPool.Put(entry)
						break loop
					} else if err != nil {
						log.Error().Err(err).Msg("[dump] entry decode error")
						dumpEntryPool.Put(entry)
						atomic.AddInt32(&errorNum, 1)
						break loop
					}

					resp, rerr := d.buildResponseFromEntry(entry)
					if rerr != nil {
						log.Error().Err(rerr).Msg("[dump] response build failed")
						dumpEntryPool.Put(entry)
						atomic.AddInt32(&errorNum, 1)
						continue loop
					}

					d.storage.Set(resp)
					dumpEntryPool.Put(entry)
					atomic.AddInt32(&successNum, 1)
				}
			}
		}(file)
	}

	wg.Wait()
	close(errCh)

	log.Info().Msgf("[dump] restored %d entries, errors: %d (elapsed: %s)", successNum, errorNum, time.Since(start))
	if errorNum > 0 {
		return fmt.Errorf("load completed with %d errors", errorNum)
	}
	return nil
}

func (d *Dump) buildResponseFromEntry(entry *dumpEntry) (*model.Response, error) {
	req := model.NewRawRequest(d.cfg, entry.MapKey, entry.ShardKey, entry.Path, entry.Query, entry.QueryHeaders)
	data := model.NewData(req.Rule(), entry.StatusCode, entry.ResponseHeaders, entry.Body)
	resp, err := model.NewResponse(data, req, d.cfg, d.backend.RevalidatorMaker(req))
	if err != nil {
		return nil, err
	}
	if resp.ShardKey() != entry.ShardKey {
		return nil, fmt.Errorf("invalid response shardKey: %d", resp.ShardKey())
	}
	if resp.MapKey() != entry.MapKey {
		return nil, fmt.Errorf("invalid response mapKey: %d", resp.MapKey())
	}
	return resp, nil
}

func extractLatestTimestamp(files []string) string {
	var timestamps []string
	for _, f := range files {
		base := filepath.Base(f)
		parts := strings.Split(base, "-")
		if len(parts) >= 4 {
			ts := strings.TrimSuffix(parts[len(parts)-1], ".dump")
			timestamps = append(timestamps, ts)
		}
	}
	sort.Strings(timestamps)
	if len(timestamps) == 0 {
		return ""
	}
	return timestamps[len(timestamps)-1]
}

func filterFilesByTimestamp(files []string, ts string) []string {
	var result []string
	for _, f := range files {
		if strings.Contains(f, ts) {
			result = append(result, f)
		}
	}
	return result
}

func getDumpFiles(dir, baseName, ext string) ([]string, error) {
	pattern := filepath.Join(dir, baseName+".*"+ext)
	files, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	return files, nil
}

func sortByModTime(files []string) ([]string, error) {
	type fileInfo struct {
		path    string
		modTime time.Time
	}
	infos := make([]fileInfo, 0, len(files))
	for _, f := range files {
		fi, err := os.Stat(f)
		if err != nil {
			continue
		}
		infos = append(infos, fileInfo{path: f, modTime: fi.ModTime()})
	}
	sort.Slice(infos, func(i, j int) bool {
		return infos[i].modTime.Before(infos[j].modTime)
	})
	sorted := make([]string, len(infos))
	for i, info := range infos {
		sorted[i] = info.path
	}
	return sorted, nil
}

func rotateOldFiles(dir, baseName, ext string, maxFiles int) error {
	files, err := getDumpFiles(dir, baseName, ext)
	if err != nil {
		return err
	}
	sorted, err := sortByModTime(files)
	if err != nil {
		return err
	}
	if len(sorted) < maxFiles {
		return nil
	}
	numToRemove := len(sorted) - (maxFiles - 1)
	for i := 0; i < numToRemove; i++ {
		if err := os.Remove(sorted[i]); err != nil {
			log.Error().Err(err).Msgf("[dump] failed to remove old dump file %s", sorted[i])
		}
	}
	return nil
}
