//go:build js && wasm

// sandbox-wasm exposes the Web Worker sandbox runner of socialweb to JavaScript.
// It is the module the page and Deno load to run a program the way a deno-kind
// compute workload runs it. Every export is asynchronous by contract: a
// synchronous wait inside a syscall/js callback would block the JavaScript
// event loop the worker's answer must arrive on, so sandboxRun returns a
// Promise that resolves with {exitCode, timedOut, stdout, stderr, err?}.
package main

import (
	"context"
	"encoding/json"
	"syscall/js"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/sandbox"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/sandbox/worker"
)

const defaultTimeout = 10 * time.Second

func main() {
	runner := worker.New()
	js.Global().Set("sandboxAvailable", js.FuncOf(func(js.Value, []js.Value) any {
		return runner.Available()
	}))
	js.Global().Set("sandboxKind", js.FuncOf(func(js.Value, []js.Value) any {
		return runner.Kind()
	}))
	js.Global().Set("sandboxRun", js.FuncOf(func(_ js.Value, args []js.Value) any {
		source := ""
		if len(args) > 0 {
			source = args[0].String()
		}
		timeout := defaultTimeout
		if len(args) > 1 && args[1].Type() == js.TypeNumber {
			timeout = time.Duration(args[1].Int()) * time.Millisecond
		}
		return promiseOf(func() (string, error) {
			result, err := runner.Run(context.Background(), sandbox.Program{Source: source, Timeout: timeout})
			out := map[string]any{"exitCode": result.ExitCode, "timedOut": result.TimedOut, "stdout": result.Stdout, "stderr": result.Stderr}
			if err != nil {
				out["err"] = err.Error()
			}
			raw, merr := json.Marshal(out)
			if merr != nil {
				return "", merr
			}
			return string(raw), nil
		})
	}))
	select {}
}

func promiseOf(run func() (string, error)) js.Value {
	var handler js.Func
	handler = js.FuncOf(func(_ js.Value, args []js.Value) any {
		resolve, reject := args[0], args[1]
		go func() {
			out, err := run()
			handler.Release()
			if err != nil {
				reject.Invoke(err.Error())
				return
			}
			resolve.Invoke(out)
		}()
		return nil
	})
	return js.Global().Get("Promise").New(handler)
}
