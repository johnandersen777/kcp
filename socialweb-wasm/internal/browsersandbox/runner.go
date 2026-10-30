//go:build js && wasm

// Package browsersandbox runs socialweb policy programs in the page's fenced
// sandbox (plan 0037): an opaque-origin iframe that starts one module Worker
// per run (scripts/browser/policy_sandbox.js), reached through the kcp
// worker's __socialwebSandbox{run, kill, probe} bridge. The browser enforces
// the fence by origin isolation, as deno does with permission flags, and the
// probe proves it before the PolicyEngine is Ready.
package browsersandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"syscall/js"
	"time"

	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/sandbox"
)

const (
	Kind = policyv1alpha1.PolicySandboxProviderBrowser

	// DefaultTimeout is spec.sandbox.browser.timeoutSeconds' default, as
	// deno's DefaultWait.
	DefaultTimeout = 2 * time.Minute

	// OutputLimit caps stdout and stderr each, as deno.OutputLimit.
	OutputLimit = 64 << 10

	ProbeTimeout = 30 * time.Second

	// grace is how long Go waits past a run's deadline for the frame's own
	// timedOut answer before it kills the run itself.
	grace = 5 * time.Second
)

var (
	_ sandbox.Runner = (*Runner)(nil)
	_ sandbox.Prober = (*Runner)(nil)
)

type Runner struct {
	bridge js.Value

	timeout time.Duration

	// slot runs one program at a time to bound iOS memory: an evaluation is a
	// sequence of Runs anyway.
	slot chan struct{}

	next atomic.Uint64

	fenced atomic.Bool
}

// Open is policy.Options.OpenSandbox for the page: only the browser provider
// runs here.
func Open(_ context.Context, spec *policyv1alpha1.PolicySandboxConfig) (sandbox.Runner, error) {
	if name := spec.ProviderName(); name != policyv1alpha1.PolicySandboxProviderBrowser {
		return nil, fmt.Errorf("browsersandbox: the page runs only the browser policy sandbox, not %s", name)
	}
	timeout := DefaultTimeout
	if spec.Browser != nil && spec.Browser.TimeoutSeconds != nil && *spec.Browser.TimeoutSeconds > 0 {
		timeout = time.Duration(*spec.Browser.TimeoutSeconds) * time.Second
	}
	return New(timeout)
}

func New(timeout time.Duration) (*Runner, error) {
	bridge := js.Global().Get("__socialwebSandbox")
	if bridge.Type() != js.TypeObject {
		return nil, errors.New("browsersandbox: the page has no policy sandbox bridge")
	}
	for _, fn := range []string{"run", "kill", "probe"} {
		if bridge.Get(fn).Type() != js.TypeFunction {
			return nil, fmt.Errorf("browsersandbox: the policy sandbox bridge has no %s", fn)
		}
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Runner{bridge: bridge, timeout: timeout, slot: make(chan struct{}, 1)}, nil
}

func (r *Runner) Kind() string { return Kind }

func (r *Runner) Available() bool { return r.bridge.Truthy() }

// Fenced is true only after Probe passed.
func (r *Runner) Fenced() bool { return r.fenced.Load() }

func (r *Runner) Probe(ctx context.Context) (sandbox.Fence, error) {
	r.fenced.Store(false)
	ctx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	text, err := await(ctx, r.bridge.Call("probe"))
	if err != nil {
		return sandbox.Fence{}, fmt.Errorf("browsersandbox: probe: %w", err)
	}
	var out struct {
		Fenced  bool   `json:"fenced"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return sandbox.Fence{}, fmt.Errorf("browsersandbox: probe answer: %w", err)
	}
	r.fenced.Store(out.Fenced)
	return sandbox.Fence{Fenced: out.Fenced, Message: out.Message}, nil
}

func (r *Runner) Run(ctx context.Context, p sandbox.Program) (sandbox.Result, error) {
	// The page only serves policy programs; a filesystem or a no-network
	// program must not get a weaker fence than it asked for.
	if p.Dir != "" {
		return sandbox.Result{}, errors.New("browsersandbox: the page sandbox has no filesystem (Program.Dir)")
	}
	if !p.Net {
		return sandbox.Result{}, errors.New("browsersandbox: the page sandbox cannot deny the network (Program.Net=false)")
	}
	if strings.TrimSpace(p.Source) == "" {
		return sandbox.Result{}, errors.New("sandbox: the program is empty")
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = r.timeout
	}
	select {
	case r.slot <- struct{}{}:
	case <-ctx.Done():
		return sandbox.Result{ExitCode: -1}, ctx.Err()
	}
	defer func() { <-r.slot }()
	id := fmt.Sprintf("r%d", r.next.Add(1))
	body, err := json.Marshal(map[string]any{"id": id, "source": p.Source, "env": p.Env, "timeoutMs": timeout.Milliseconds()})
	if err != nil {
		return sandbox.Result{}, err
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout+grace)
	defer cancel()
	text, err := await(runCtx, r.bridge.Call("run", string(body)))
	if err != nil {
		r.bridge.Call("kill", id)
		if ctx.Err() != nil {
			return sandbox.Result{ExitCode: -1}, ctx.Err()
		}
		if runCtx.Err() != nil {
			return sandbox.Result{ExitCode: -1, TimedOut: true}, nil
		}
		return sandbox.Result{ExitCode: -1}, fmt.Errorf("browsersandbox: run: %w", err)
	}
	var out struct {
		ExitCode int    `json:"exitCode"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
		TimedOut bool   `json:"timedOut"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return sandbox.Result{ExitCode: -1}, fmt.Errorf("browsersandbox: run answer: %w", err)
	}
	return sandbox.Result{ExitCode: out.ExitCode, TimedOut: out.TimedOut, Stdout: capped(out.Stdout), Stderr: capped(out.Stderr)}, nil
}

func capped(s string) string {
	if len(s) > OutputLimit {
		return s[:OutputLimit]
	}
	return s
}

// await waits for a page Promise of a string from a goroutine (never from a
// js.FuncOf callback; socialweb CONTRIBUTING "wasm").
func await(ctx context.Context, promise js.Value) (string, error) {
	type result struct {
		text string
		err  error
	}
	done := make(chan result, 1)
	then := js.FuncOf(func(_ js.Value, args []js.Value) any {
		done <- result{text: args[0].String()}
		return nil
	})
	catch := js.FuncOf(func(_ js.Value, args []js.Value) any {
		done <- result{err: errors.New(js.Global().Get("String").Invoke(args[0]).String())}
		return nil
	})
	promise.Call("then", then, catch)
	select {
	case r := <-done:
		then.Release()
		catch.Release()
		return r.text, r.err
	case <-ctx.Done():
		// ponytail: then/catch stay unreleased (two js.Funcs per abandoned
		// run): the page settles the Promise later, and a released func
		// called from JS panics the Go runtime.
		return "", ctx.Err()
	}
}
