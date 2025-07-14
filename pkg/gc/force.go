package gc

import (
	"context"
	"fmt"
	"github.com/Borislavv/advanced-cache/pkg/config"
	"runtime"
	"runtime/debug"
	"time"

	"github.com/rs/zerolog/log"
)

// Run periodically forces Go's garbage collector and tries to return freed pages back to the OS.
// ----------------------------------------------
// Why is this needed?
//
// This service is a high-load in-memory cache.
// Once the cache reaches its target size (e.g., 10-20 million keys),
// the heap stabilizes at a large size — for example, 18 GB.
// By default, Go's GC will only run a full collection if the heap grows by GOGC% (default 100%).
// This means the next GC cycle could be delayed until the heap doubles again (e.g., 36 GB).
//
// For a cache, this almost never happens: the cache keeps its "critical mass" and rarely doubles in size,
// but in the meantime, temporary buffers and evicted objects create garbage.
// If no GC happens, this garbage is never reclaimed, so the process appears to "leak" memory.
//
// To prevent this, we force `runtime.GC()` on a short interval,
// and periodically call `debug.FreeOSMemory()` to push freed pages back to the OS.
// Both intervals are configurable in the config.
//
// This guarantees:
//   - predictable and stable memory usage
//   - less surprise RSS growth during steady state
//   - smoother operation for long-lived caches under high load.
func Run(ctx context.Context, cfg *config.Cache) {
	go func() {
		// Force GC walk-through every cfg.Cache.ForceGC.GCInterval
		gcTicker := time.NewTicker(cfg.Cache.ForceGC.GCInterval)
		defer gcTicker.Stop()

		// Return free pages to OS every cfg.Cache.ForceGC.FreeOsMemInterval
		freeOssMemTicker := time.NewTicker(cfg.Cache.ForceGC.FreeOsMemInterval)
		defer freeOssMemTicker.Stop()

		log.Info().Msgf(
			"[force-GC] running with gcInterval=%s, freeOsMemInterval=%s",
			cfg.Cache.ForceGC.GCInterval, cfg.Cache.ForceGC.FreeOsMemInterval,
		)

		var lastAlloc uint64

		for {
			select {
			case <-ctx.Done():
				log.Info().Msg("[force-GC] stopped")
				return

			case <-gcTicker.C:
				var mem runtime.MemStats
				runtime.ReadMemStats(&mem)

				runtime.GC()

				log.Info().Msgf(
					"[force-GC] forced GC pass (last GC pass at: %s, pause: %s)",
					time.Unix(0, int64(mem.LastGC)).Format(time.RFC3339Nano),
					lastGCPauseNs(mem.PauseNs),
				)

				lastAlloc = mem.Alloc
			case <-freeOssMemTicker.C:
				var mem runtime.MemStats
				runtime.ReadMemStats(&mem)

				if lastAlloc == 0 {
					lastAlloc = mem.Alloc
					continue
				}

				debug.FreeOSMemory() // use madvise(DONTNEED) under the hood

				log.Info().Msgf(
					"[force-GC] forcing flush of freed memory to OS (alloc was %s, now %s)",
					fmtBytes(lastAlloc), fmtBytes(mem.Alloc),
				)

				lastAlloc = mem.Alloc
			}
		}
	}()
}

// fmtBytes formats a byte count to a human-readable string.
func fmtBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func lastGCPauseNs(pauses [256]uint64) time.Duration {
	for i := 255; i >= 0; i-- {
		if pauses[i] > 0 {
			return time.Duration(pauses[i])
		}
	}
	return time.Duration(0)
}
