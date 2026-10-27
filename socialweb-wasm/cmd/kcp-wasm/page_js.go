//go:build js && wasm

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"syscall/js"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/sandbox"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/sandbox/worker"

	"github.com/johnandersen777/socialweb-computer-kcp/wasm/internal/browserinfra/exposure"
	"github.com/johnandersen777/socialweb-computer-kcp/wasm/internal/socialweb"
)

// defaultIngressProxyHost is the xrpc-subscription relay prod uses.
const defaultIngressProxyHost = "xrpc.fedproxy.com"

// registerPageExports installs the Go functions only the page reaches, through
// kcp_worker.js "socialweb-call" messages. None of them is routed from
// socialwebHandleRequest, an Exposure, or the relay: a relay request frame
// reaches Go only via socialwebIngressRequest, which serves nothing but the
// ingress Exposure routes of its host.
//
// Each takes (requestJSON, callbackID) and answers through
// __kcpResolve(callbackID, responseJSON) like kcpHandleRequest.
func registerPageExports(orc *socialweb.Orchestrator) {
	registerSandboxExports()
	pageExport("socialwebVaultPut", func(raw string) any {
		var in socialweb.ContractSecrets
		if err := json.Unmarshal([]byte(raw), &in); err != nil {
			return failure(http.StatusBadRequest, err)
		}
		if err := orc.PutContractSecrets(context.Background(), in); err != nil {
			return failure(http.StatusBadRequest, err)
		}
		return map[string]any{"status": http.StatusOK}
	})
	pageExport("socialwebSignOut", func(raw string) any {
		var in struct {
			Workspace string `json:"workspace"`
			Name      string `json:"name"`
		}
		if err := json.Unmarshal([]byte(raw), &in); err != nil {
			return failure(http.StatusBadRequest, err)
		}
		if err := orc.Identity.SignOut(context.Background(), in.Workspace, in.Name); err != nil {
			return failure(http.StatusBadRequest, err)
		}
		return map[string]any{"status": http.StatusOK}
	})
	pageExport("socialwebIngressSign", func(raw string) any {
		var in struct {
			Host  string `json:"host"`
			Nonce string `json:"nonce"`
		}
		if err := json.Unmarshal([]byte(raw), &in); err != nil {
			return failure(http.StatusBadRequest, err)
		}
		nonce, err := base64.StdEncoding.DecodeString(in.Nonce)
		if err != nil {
			return failure(http.StatusBadRequest, err)
		}
		sig, err := orc.Exposure.SignIngress(in.Host, nonce)
		if err != nil {
			return failure(http.StatusNotFound, err)
		}
		return map[string]any{"status": http.StatusOK, "signature": base64.StdEncoding.EncodeToString(sig)}
	})
	pageExport("socialwebIngressToken", func(raw string) any {
		var in struct {
			Host string `json:"host"`
			NSID string `json:"nsid"`
		}
		if err := json.Unmarshal([]byte(raw), &in); err != nil {
			return failure(http.StatusBadRequest, err)
		}
		token, err := orc.Exposure.IngressServiceAuth(in.Host, in.NSID)
		if err != nil {
			return failure(http.StatusNotFound, err)
		}
		return map[string]any{"status": http.StatusOK, "token": token}
	})
	pageExport("socialwebIngressRequest", func(raw string) any {
		var in exposure.IngressRequest
		if err := json.Unmarshal([]byte(raw), &in); err != nil {
			return failure(http.StatusBadRequest, err)
		}
		return orc.Exposure.ServeIngress(context.Background(), in)
	})
	// A page that resumed this program from a memory snapshot has no relay
	// subscriptions yet; it asks for the current list instead of waiting for
	// the next change.
	pageExport("socialwebIngressList", func(string) any {
		return map[string]any{"status": http.StatusOK, "ingress": orc.Exposure.Ingresses()}
	})
	if fn := js.Global().Get("__socialwebIngress"); fn.Type() == js.TypeFunction {
		raw, _ := json.Marshal(orc.Exposure.Ingresses())
		fn.Invoke(string(raw))
	}
}

func pageExport(name string, fn func(raw string) any) {
	js.Global().Set(name, js.FuncOf(func(_ js.Value, args []js.Value) any {
		if len(args) < 2 {
			return nil
		}
		raw, id := args[0].String(), args[1].Int()
		go func() {
			out, _ := json.Marshal(fn(raw))
			js.Global().Call("__kcpResolve", id, string(out))
		}()
		return nil
	}))
}

// registerSandboxExports exposes the Web Worker sandbox runner: the same
// sandbox.Runner a deno-kind compute workload runs under, executed in the page
// instead of a deno process. The answer arrives through __kcpResolve like every
// page export, because a synchronous wait would block the event loop the
// worker's message must arrive on.
func registerSandboxExports() {
	runner := worker.New()
	js.Global().Set("socialwebSandboxAvailable", js.FuncOf(func(js.Value, []js.Value) any {
		return runner.Available()
	}))
	pageExport("socialwebSandboxRun", func(raw string) any {
		var in struct {
			Source    string `json:"source"`
			TimeoutMs int    `json:"timeoutMs"`
		}
		if err := json.Unmarshal([]byte(raw), &in); err != nil {
			return failure(400, err)
		}
		timeout := 10 * time.Second
		if in.TimeoutMs > 0 {
			timeout = time.Duration(in.TimeoutMs) * time.Millisecond
		}
		result, err := runner.Run(context.Background(), sandbox.Program{Source: in.Source, Timeout: timeout})
		out := map[string]any{"exitCode": result.ExitCode, "timedOut": result.TimedOut, "stdout": result.Stdout, "stderr": result.Stderr}
		if err != nil {
			out["error"] = err.Error()
		}
		return out
	})
}

func failure(status int, err error) map[string]any {
	return map[string]any{"status": status, "error": err.Error()}
}
