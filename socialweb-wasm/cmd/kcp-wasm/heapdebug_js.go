//go:build js && wasm

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"runtime"
	"runtime/pprof"
	"strconv"
	"syscall/js"
	"time"
)

// startHeapDebug is debug-only: with KCP_HEAP_PROFILE=<seconds> in the Go env
// it hands the host __kcpHeapDump(memStatsJSON, heapProfileBase64) on that
// period. The default (unset) keeps allocation profiling off.
func startHeapDebug() {
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
			if fn := js.Global().Get("__kcpHeapDump"); fn.Type() == js.TypeFunction {
				fn.Invoke(string(stats), base64.StdEncoding.EncodeToString(buf.Bytes()))
			}
		}
	}()
}
