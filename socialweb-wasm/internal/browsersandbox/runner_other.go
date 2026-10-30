//go:build !js || !wasm

package browsersandbox

import (
	"context"
	"errors"

	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/sandbox"
)

// Open fails off the page: a native host never pretends to run the browser
// sandbox.
func Open(context.Context, *policyv1alpha1.PolicySandboxConfig) (sandbox.Runner, error) {
	return nil, errors.New("browsersandbox: the browser policy sandbox needs js/wasm")
}
