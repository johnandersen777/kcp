//go:build js && wasm

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"syscall/js"
	"time"

	"github.com/kcp-dev/kcp/contrib/wasm/storage"
)

// startHeapDebug is debug-only: with KCP_HEAP_PROFILE=<seconds> in the Go env
// it hands the host __kcpHeapDump(memStatsJSON, heapProfileBase64, goroutineProfileBase64) on that
// period. The default (unset) keeps allocation profiling off.
func startHeapDebug() {
	startMemStats()
	period, _ := strconv.Atoi(os.Getenv("KCP_HEAP_PROFILE"))
	if period <= 0 {
		// Allocation profiling samples callers through the wasm pclntab; with
		// this module size the js runtime occasionally reports an invalid
		// symbol table and aborts. The browser has no use for heap profiles.
		runtime.MemProfileRate = 0
		return
	}
	go func() {
		for range time.Tick(time.Duration(period) * time.Second) {
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			stats, _ := json.Marshal(ms)
			var buf bytes.Buffer
			_ = pprof.WriteHeapProfile(&buf)
			var goroutines bytes.Buffer
			_ = pprof.Lookup("goroutine").WriteTo(&goroutines, 0)
			if fn := js.Global().Get("__kcpHeapDump"); fn.Type() == js.TypeFunction {
				fn.Invoke(string(stats), base64.StdEncoding.EncodeToString(buf.Bytes()),
					base64.StdEncoding.EncodeToString(goroutines.Bytes()))
			}
		}
	}()
}

// startMemStats is debug-only and cheap: with KCP_MEMSTATS=<seconds> it logs
// one line of runtime.MemStats and the goroutine count on that period, so a
// slow heap growth on a phone shows whether the live heap, idle spans or
// goroutines are what grows.
func startMemStats() {
	period, _ := strconv.Atoi(os.Getenv("KCP_MEMSTATS"))
	if period <= 0 {
		return
	}
	go func() {
		for range time.Tick(time.Duration(period) * time.Second) {
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			mib := func(b uint64) float64 { return float64(b) / (1 << 20) }
			fmt.Fprintf(os.Stderr, "memstats: sys=%.1fMiB heapSys=%.1fMiB heapInuse=%.1fMiB heapIdle=%.1fMiB heapReleased=%.1fMiB heapObjects=%d stackInuse=%.1fMiB goroutines=%d numGC=%d nextGC=%.1fMiB totalAlloc=%.0fMiB mallocs=%d gcPauseTotal=%s gcCPU=%.3f\n",
				mib(ms.Sys), mib(ms.HeapSys), mib(ms.HeapInuse), mib(ms.HeapIdle), mib(ms.HeapReleased),
				ms.HeapObjects, mib(ms.StackInuse), runtime.NumGoroutine(), ms.NumGC, mib(ms.NextGC),
				mib(ms.TotalAlloc), ms.Mallocs, time.Duration(ms.PauseTotalNs).Round(time.Millisecond), ms.GCCPUFraction)
			// Updates per resource since the last line, top 8: written/no-op.
			counts := storage.TakeUpdateCounts()
			names := make([]string, 0, len(counts))
			for name := range counts {
				names = append(names, name)
			}
			sort.Slice(names, func(i, j int) bool {
				a, b := counts[names[i]], counts[names[j]]
				return a[0]+a[1] > b[0]+b[1]
			})
			if len(names) > 8 {
				names = names[:8]
			}
			parts := make([]string, 0, len(names))
			for _, name := range names {
				parts = append(parts, fmt.Sprintf("%s=%d/%d", name, counts[name][0], counts[name][1]))
			}
			fmt.Fprintf(os.Stderr, "updates (written/noop per resource): %s\n", strings.Join(parts, " "))
		}
	}()
}
