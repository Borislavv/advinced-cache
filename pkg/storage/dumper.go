package storage

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/Borislavv/advanced-cache/pkg/mock"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Borislavv/advanced-cache/pkg/config"
	"github.com/Borislavv/advanced-cache/pkg/model"
	"github.com/Borislavv/advanced-cache/pkg/repository"
	sharded "github.com/Borislavv/advanced-cache/pkg/storage/map"
	"github.com/rs/zerolog/log"
)

var (
	errDumpNotEnabled = errors.New("persistence mode is not enabled")
)

type Dumper interface {
	Dump(ctx context.Context) error
	Load(ctx context.Context) error
}

type Dump struct {
	cfg        *config.Cache
	shardedMap *sharded.Map[*model.Entry]
	storage    Storage
	backend    repository.Backender
}

func NewDumper(cfg *config.Cache, sm *sharded.Map[*model.Entry], storage Storage, backend repository.Backender) *Dump {
	return &Dump{
		cfg:        cfg,
		shardedMap: sm,
		storage:    storage,
		backend:    backend,
	}
}

func (d *Dump) Dump(ctx context.Context) error {
	start := time.Now()

	cfg := d.cfg.Cache.Persistence.Dump
	if !cfg.IsEnabled {
		return errDumpNotEnabled
	}

	// Ensure base dir exists
	if err := os.MkdirAll(cfg.Dir, 0755); err != nil {
		return fmt.Errorf("create base dump dir: %w", err)
	}

	// Determine new version dir
	version := nextVersionDir(cfg.Dir)
	versionDir := filepath.Join(cfg.Dir, fmt.Sprintf("v%d", version))
	if err := os.MkdirAll(versionDir, 0755); err != nil {
		return fmt.Errorf("create version dir: %w", err)
	}

	timestamp := time.Now().Format("20060102T150405")
	var wg sync.WaitGroup
	var successNum, errorNum int32

	d.shardedMap.WalkShards(func(shardKey uint64, shard *sharded.Shard[*model.Entry]) {
		wg.Add(1)
		go func(shardKey uint64) {
			defer wg.Done()

			filename := fmt.Sprintf("%s/%s-shard-%d-%s.dump",
				versionDir, cfg.Name, shardKey, timestamp)
			tmpName := filename + ".tmp"

			f, err := os.Create(tmpName)
			if err != nil {
				log.Error().Err(err).Msg("[dump] create error")
				atomic.AddInt32(&errorNum, 1)
				return
			}
			defer f.Close()

			bw := bufio.NewWriterSize(f, 512*1024)

			shard.Walk(ctx, func(key uint64, entry *model.Entry) bool {
				data, releaser := entry.ToBytes()
				defer releaser()

				var scratch [4]byte
				binary.LittleEndian.PutUint32(scratch[:], uint32(len(data)))
				bw.Write(scratch[:])
				bw.Write(data)

				atomic.AddInt32(&successNum, 1)

				return true
			}, true)

			bw.Flush()
			_ = f.Close()
			_ = os.Rename(tmpName, filename)
		}(shardKey)
	})

	wg.Wait()

	rotateVersionDirs(cfg.Dir, 2)

	log.Info().Msgf("[dump] finished: %d entries, errors: %d, elapsed: %s",
		successNum, errorNum, time.Since(start))
	if errorNum > 0 {
		return fmt.Errorf("dump finished with %d errors", errorNum)
	}

	return nil
}

func (d *Dump) Load(ctx context.Context) error {
	var successNum int32
	defer func() {
		if successNum == 0 {
			go func() {
				log.Info().Msg("[dump] dump restored 0 keys, mock data start loading")
				defer log.Info().Msg("[dump] mocked data finished loading")
				path := []byte("/api/v2/pagedata")
				for entry := range mock.StreamSeqEntries(ctx, d.cfg, d.backend, path, 10_000_000) {
					d.storage.Set(entry)
				}
			}()
		}
	}()

	start := time.Now()

	cfg := d.cfg.Cache.Persistence.Dump
	if !cfg.IsEnabled {
		return errDumpNotEnabled
	}

	// Find latest version directory
	latestVersionDir := getLatestVersionDir(cfg.Dir)
	if latestVersionDir == "" {
		return fmt.Errorf("no versioned dump dirs found in %s", cfg.Dir)
	}

	files, err := filepath.Glob(filepath.Join(latestVersionDir, fmt.Sprintf("%s-shard-*.dump", cfg.Name)))
	if err != nil {
		return fmt.Errorf("glob error: %w", err)
	}
	if len(files) == 0 {
		return fmt.Errorf("no dump files found in %s", latestVersionDir)
	}

	latestTs := extractLatestTimestamp(files)
	filesToLoad := filterFilesByTimestamp(files, latestTs)

	var wg sync.WaitGroup
	var errorNum int32

	for _, file := range filesToLoad {
		wg.Add(1)
		go func(file string) {
			defer wg.Done()

			f, err := os.Open(file)
			if err != nil {
				log.Error().Err(err).Msg("[load] open error")
				atomic.AddInt32(&errorNum, 1)
				return
			}
			defer f.Close()

			br := bufio.NewReaderSize(f, 512*1024)
			var sizeBuf [4]byte
			for {
				if _, err := io.ReadFull(br, sizeBuf[:]); err == io.EOF {
					break
				} else if err != nil {
					log.Error().Err(err).Msg("[load] read size error")
					atomic.AddInt32(&errorNum, 1)
					break
				}
				size := binary.LittleEndian.Uint32(sizeBuf[:])
				buf := make([]byte, size)
				if _, err := io.ReadFull(br, buf); err != nil {
					log.Error().Err(err).Msg("[load] read entry error")
					atomic.AddInt32(&errorNum, 1)
					break
				}
				entry, err := model.EntryFromBytes(buf, d.cfg, d.backend)
				if err != nil {
					log.Error().Err(err).Msg("[load] entry decode error")
					atomic.AddInt32(&errorNum, 1)
					continue
				}
				d.storage.Set(entry)
				atomic.AddInt32(&successNum, 1)

				select {
				case <-ctx.Done():
					return
				default:
				}
			}
		}(file)
	}

	wg.Wait()
	log.Info().Msgf("[dump] restored: %d entries, errors: %d, elapsed: %s",
		successNum, errorNum, time.Since(start))
	if errorNum > 0 {
		return fmt.Errorf("load finished with %d errors", errorNum)
	}

	return nil
}

// Determine next version dir number.
func nextVersionDir(baseDir string) int {
	entries, _ := filepath.Glob(filepath.Join(baseDir, "v*"))
	maxV := 0
	for _, dir := range entries {
		base := filepath.Base(dir)
		if !strings.HasPrefix(base, "v") {
			continue
		}
		var v int
		fmt.Sscanf(base, "v%d", &v)
		if v > maxV {
			maxV = v
		}
	}
	return maxV + 1
}

// Rotate directories: keep only the latest N version dirs.
func rotateVersionDirs(baseDir string, max int) {
	entries, _ := filepath.Glob(filepath.Join(baseDir, "v*"))
	if len(entries) <= max {
		return
	}
	sort.Slice(entries, func(i, j int) bool {
		fi, _ := os.Stat(entries[i])
		fj, _ := os.Stat(entries[j])
		return fi.ModTime().After(fj.ModTime())
	})
	for _, dir := range entries[max:] {
		os.RemoveAll(dir)
		log.Info().Msgf("[dump] removed old dump dir: %s", dir)
	}
}

// Find latest version dir name.
func getLatestVersionDir(baseDir string) string {
	entries, _ := filepath.Glob(filepath.Join(baseDir, "v*"))
	if len(entries) == 0 {
		return ""
	}
	sort.Slice(entries, func(i, j int) bool {
		fi, _ := os.Stat(entries[i])
		fj, _ := os.Stat(entries[j])
		return fi.ModTime().After(fj.ModTime())
	})
	return entries[0]
}

func extractLatestTimestamp(files []string) string {
	var tsList []string
	for _, f := range files {
		parts := strings.Split(filepath.Base(f), "-")
		if len(parts) >= 4 {
			ts := strings.TrimSuffix(parts[len(parts)-1], ".dump")
			tsList = append(tsList, ts)
		}
	}
	sort.Strings(tsList)
	if len(tsList) == 0 {
		return ""
	}
	return tsList[len(tsList)-1]
}

func filterFilesByTimestamp(files []string, ts string) []string {
	var out []string
	for _, f := range files {
		if strings.Contains(f, ts) {
			out = append(out, f)
		}
	}
	return out
}
