//go:build js && wasm

// Page half of the policy suite (scripts/browser-dev/policy-suite.sh); names
// match socialweb's policy tests. Runs under node:
//
//	GOOS=js GOARCH=wasm go test -modfile=<wasm modfile> ./internal/browsersandbox
package browsersandbox

import (
	"context"
	"strings"
	"syscall/js"
	"testing"

	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/sandbox"
)

const bridgeName = "__socialwebSandbox"

// fakeBridge installs a __socialwebSandbox whose probe answers probeJSON and
// whose run counts calls.
func fakeBridge(t *testing.T, probeJSON string, runs *int) {
	t.Helper()
	resolve := func(v string) any { return js.Global().Get("Promise").Call("resolve", v) }
	fns := map[string]js.Func{
		"probe": js.FuncOf(func(js.Value, []js.Value) any { return resolve(probeJSON) }),
		"run": js.FuncOf(func(js.Value, []js.Value) any {
			*runs++
			return resolve(`{"exitCode":0,"stdout":"ok\n","stderr":"","timedOut":false}`)
		}),
		"kill": js.FuncOf(func(js.Value, []js.Value) any { return nil }),
	}
	obj := js.Global().Get("Object").New()
	for name, fn := range fns {
		obj.Set(name, fn)
	}
	js.Global().Set(bridgeName, obj)
	t.Cleanup(func() {
		js.Global().Delete(bridgeName)
		for _, fn := range fns {
			fn.Release()
		}
	})
}

// provider-refused: the page opens only the browser provider; nil (deno by
// default), deno and denoworker are refused.
func TestProviderRefused(t *testing.T) {
	for _, spec := range []*policyv1alpha1.PolicySandboxConfig{nil, {Provider: "deno"}, {Provider: "denoworker"}, {Provider: "none"}} {
		if r, err := Open(context.Background(), spec); err == nil || r != nil {
			t.Errorf("Open(%v) = %v, %v; want refusal", spec, r, err)
		}
	}
}

// sandbox-unavailable-deny: no bridge, or a bridge missing run/kill/probe,
// gives no Runner.
func TestSandboxUnavailableDeny(t *testing.T) {
	js.Global().Delete(bridgeName)
	if _, err := New(0); err == nil {
		t.Fatal("New without a bridge succeeded")
	}
	partial := js.Global().Get("Object").New()
	partial.Set("run", js.FuncOf(func(js.Value, []js.Value) any { return nil }))
	js.Global().Set(bridgeName, partial)
	defer js.Global().Delete(bridgeName)
	if _, err := New(0); err == nil || !strings.Contains(err.Error(), "kill") {
		t.Fatalf("New with only run = %v; want missing kill", err)
	}
}

// sandbox-unfenced-deny (page half): Fenced is false until a probe passes and
// drops back to false when a later probe fails; the engine gates runs on it
// (socialweb expression.go, controller manager.go).
func TestSandboxUnfencedDeny(t *testing.T) {
	var runs int
	for _, tc := range []struct {
		probe  string
		fenced bool
	}{
		{`{"fenced":false,"message":"idb=VISIBLE"}`, false},
		{`not json`, false},
		{`{"fenced":true,"message":"origin=null"}`, true},
		{`{"fenced":false,"message":"parent=READABLE"}`, false},
	} {
		fakeBridge(t, tc.probe, &runs)
		r, err := Open(context.Background(), &policyv1alpha1.PolicySandboxConfig{Provider: policyv1alpha1.PolicySandboxProviderBrowser})
		if err != nil {
			t.Fatal(err)
		}
		if r.Fenced() {
			t.Fatalf("%s: fenced before any probe", tc.probe)
		}
		_, _ = r.(sandbox.Prober).Probe(context.Background())
		if r.Fenced() != tc.fenced {
			t.Fatalf("probe %s: Fenced() = %v, want %v", tc.probe, r.Fenced(), tc.fenced)
		}
	}
	if runs != 0 {
		t.Fatalf("probing ran %d programs through run", runs)
	}
}

// program-shape-refused: the page sandbox has no filesystem and cannot deny
// the network, so programs asking for either (and empty programs) never run.
func TestProgramShapeRefused(t *testing.T) {
	var runs int
	fakeBridge(t, `{"fenced":true}`, &runs)
	r, err := New(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []sandbox.Program{
		{Source: `console.log(1)`, Dir: "/work", Net: true},
		{Source: `console.log(1)`, Net: false},
		{Source: "  \n", Net: true},
	} {
		if _, err := r.Run(context.Background(), p); err == nil {
			t.Errorf("Run(%+v) succeeded", p)
		}
	}
	if runs != 0 {
		t.Fatalf("%d refused programs reached the bridge", runs)
	}
	if res, err := r.Run(context.Background(), sandbox.Program{Source: `console.log("ok")`, Net: true}); err != nil || res.Stdout != "ok\n" || runs != 1 {
		t.Fatalf("valid program: %+v %v runs=%d", res, err, runs)
	}
}
