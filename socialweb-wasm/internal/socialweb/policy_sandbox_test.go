package socialweb

import (
	"testing"

	policyv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/policy/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
)

// The composed PolicyEngine gets spec.sandbox.provider browser from the
// MarketRequester's spec.components (plan 0037).
func TestBrowserPolicySandboxOverride(t *testing.T) {
	components := browserPolicySandbox()
	if !hasBrowserPolicySandbox(components) {
		t.Fatal("hasBrowserPolicySandbox is false for browserPolicySandbox()")
	}
	if hasBrowserPolicySandbox(nil) {
		t.Fatal("hasBrowserPolicySandbox is true without components")
	}
	out, err := compose.ApplyOverrides(components, []compose.Desired{compose.PolicyEngineDesired()})
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := out[0].Spec.(map[string]any)
	sandbox, _ := spec["sandbox"].(map[string]any)
	if sandbox["provider"] != policyv1alpha1.PolicySandboxProviderBrowser {
		t.Fatalf("PolicyEngine spec = %v, want sandbox.provider browser", spec)
	}
}
