//go:build js && wasm

/*
Copyright 2026 The KCP Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Command kubectl-wasm builds upstream kubectl (k8s.io/kubectl/pkg/cmd) for
// GOOS=js GOARCH=wasm and exposes it to a JavaScript host:
//
//	kubectlRun(requestJSON, callbackID) -> __kubectlResolve(callbackID, resultJSON)
//
// The request carries kubectl arguments and optional stdin; the result carries
// stdout, stderr and the exit code. kubectl's HTTP client uses Go's js/wasm
// net/http transport, which delegates every request to the host's fetch().
package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"syscall/js"

	"k8s.io/cli-runtime/pkg/genericiooptions"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
)

// fatalExit is panicked by kubectl's fatal-error handler instead of calling
// os.Exit, which would terminate the WASM runtime before the result could be
// returned. Commands routinely call cmdutil.CheckErr from Run, so this is a
// normal error path, not an exceptional one.
type fatalExit struct {
	message string
	code    int
}

func (e fatalExit) Error() string { return e.message }

type runRequest struct {
	Args  []string `json:"args"`
	Stdin string   `json:"stdin"`
}

type runResult struct {
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	Code   int    `json:"code"`
}

func main() {
	handle := js.FuncOf(func(this js.Value, args []js.Value) any {
		if len(args) < 2 {
			return nil
		}
		requestJSON := args[0].String()
		id := args[1].Int()

		// Run outside the JS callback: kubectl's requests are asynchronous on
		// js/wasm, and blocking the callback would prevent fetch from
		// completing.
		go func() {
			js.Global().Call("__kubectlResolve", id, run(requestJSON))
		}()
		return nil
	})

	js.Global().Set("kubectlRun", handle)
	js.Global().Set("__kubectlWasmReady", true)
	js.Global().Get("console").Call("error", "kubectl-wasm: registered kubectlRun")
	select {}
}

func run(requestJSON string) (resultJSON string) {
	var req runRequest
	var out, errOut strings.Builder

	result := runResult{Code: 1}
	defer func() {
		if r := recover(); r != nil {
			if exit, ok := r.(fatalExit); ok {
				if exit.message != "" {
					errOut.WriteString(exit.message)
					if !strings.HasSuffix(exit.message, "\n") {
						errOut.WriteString("\n")
					}
				}
				result = runResult{
					Stdout: out.String(),
					Stderr: errOut.String(),
					Code:   exit.code,
				}
			} else {
				fmt.Fprintf(&errOut, "panic: %v\n", r)
				result = runResult{Stdout: out.String(), Stderr: errOut.String(), Code: 1}
			}
		}
		resultJSON = marshal(result)
	}()

	if err := json.Unmarshal([]byte(requestJSON), &req); err != nil {
		errOut.WriteString("invalid request: " + err.Error() + "\n")
		result = runResult{Stdout: out.String(), Stderr: errOut.String(), Code: 1}
		return
	}

	streams := genericiooptions.IOStreams{
		In:     strings.NewReader(req.Stdin),
		Out:    &out,
		ErrOut: &errOut,
	}

	// kubectl treats many user errors as fatal and calls os.Exit. In WASM
	// that kills the runtime before __kubectlResolve runs; turn it into a
	// panic that run() recovers into a normal result.
	cmdutil.BehaviorOnFatal(func(message string, code int) {
		panic(fatalExit{message: message, code: code})
	})

	command := newKubectlCommand(streams, req.Args)
	command.SetArgs(req.Args)
	command.SetIn(streams.In)
	command.SetOut(&out)
	command.SetErr(&errOut)

	code := 0
	if err := command.Execute(); err != nil {
		code = 1
		// Cobra prints the error to command.ErrOrStderr unless silenced, but
		// commands that return errors without going through cobra (rare) would
		// otherwise be silent.
		if errOut.Len() == 0 || !strings.Contains(errOut.String(), err.Error()) {
			fmt.Fprintln(&errOut, "error:", err)
		}
	}

	result = runResult{Stdout: out.String(), Stderr: errOut.String(), Code: code}
	return
}

func marshal(result runResult) string {
	raw, err := json.Marshal(result)
	if err != nil {
		return fmt.Sprintf(`{"stdout":"","stderr":%q,"code":1}`, err.Error())
	}
	return string(raw)
}
