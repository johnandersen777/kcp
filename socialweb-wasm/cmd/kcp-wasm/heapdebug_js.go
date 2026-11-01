//go:build js && wasm

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"syscall/js"
	"time"

	kruntime "k8s.io/apimachinery/pkg/runtime"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"

	"github.com/kcp-dev/kcp/contrib/wasm/storage"
)

// startHeapDebug is debug-only: with KCP_HEAP_PROFILE=<seconds> in the Go env
// it hands the host __kcpHeapDump(memStatsJSON, heapProfileBase64, goroutineProfileBase64) on that
// period. The default (unset) keeps allocation profiling off.
func startHeapDebug() {
	startMemStats()
	startNoopLog()
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

// startNoopLog is debug-only: with KCP_NOOP_LOG=1 it logs one line per
// update, no-op or written (key, verb, subresource, user): which controller
// re-applies unchanged objects, and what an idle page keeps writing.
// Noisy; off by default.
func startNoopLog() {
	if os.Getenv("KCP_NOOP_LOG") != "1" {
		return
	}
	storage.UpdateLog = func(ctx context.Context, key string, written bool) {
		verb, sub, user := "", "", ""
		if ri, ok := genericapirequest.RequestInfoFrom(ctx); ok {
			verb, sub = ri.Verb, ri.Subresource
		}
		if u, ok := genericapirequest.UserFrom(ctx); ok {
			user = u.GetName()
		}
		kind := "noop"
		if written {
			kind = "written"
		}
		fmt.Fprintf(os.Stderr, "update %s: %s verb=%s subresource=%s user=%s\n", kind, key, verb, sub, user)
	}
	// The field paths a written update changed (values never logged), so a
	// churn line names the status field a controller keeps rewriting.
	storage.UpdateDiffLog = func(_ context.Context, key string, before, after kruntime.Object) {
		var a, b any
		if raw, err := json.Marshal(before); err == nil {
			_ = json.Unmarshal(raw, &a)
		}
		if raw, err := json.Marshal(after); err == nil {
			_ = json.Unmarshal(raw, &b)
		}
		var paths []string
		changedPaths("", a, b, 4, &paths)
		fmt.Fprintf(os.Stderr, "update fields: %s %s\n", key, strings.Join(paths, ","))
	}
}

// changedPaths appends the JSON paths where a and b differ, down to depth
// levels (deeper differences are reported at that level). metadata
// resourceVersion and managedFields always change with a write and are
// skipped.
func changedPaths(path string, a, b any, depth int, out *[]string) {
	// By last segment: internal types without JSON tags (a Lease) inline
	// their ObjectMeta, so these sit at the top level there.
	switch path[strings.LastIndex(path, ".")+1:] {
	case "resourceVersion", "managedFields", "ResourceVersion", "ManagedFields":
		return
	}
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok && depth > 0 {
		keys := map[string]bool{}
		for k := range am {
			keys[k] = true
		}
		for k := range bm {
			keys[k] = true
		}
		sorted := make([]string, 0, len(keys))
		for k := range keys {
			sorted = append(sorted, k)
		}
		sort.Strings(sorted)
		for _, k := range sorted {
			changedPaths(path+"."+k, am[k], bm[k], depth-1, out)
		}
		return
	}
	if !reflect.DeepEqual(a, b) {
		*out = append(*out, path)
	}
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
