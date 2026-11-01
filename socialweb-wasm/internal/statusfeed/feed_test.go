package statusfeed

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestWebRTCProjection(t *testing.T) {
	c := contract(&unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "c"},
		"status": map[string]any{"contract": map[string]any{
			"networks": []any{map[string]any{"tag": "ssh", "kind": "webrtc", "address": "sha-256 AB"}},
			"hostKeys": map[string]any{"keys": []any{map[string]any{"type": "ssh-ed25519", "publicKey": "AAAA"}}},
		}},
	}})
	if len(c.Networks) != 1 || c.Networks[0].Kind != "webrtc" || len(c.HostKeys) != 1 || c.HostKeys[0] != "ssh-ed25519 AAAA" {
		t.Fatalf("contract projection: %+v", c)
	}
	event := func(direction string) ContractEvent {
		return contractEvent(&unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "e"},
			"spec": map[string]any{
				"contractRef": map[string]any{"name": "c"}, "direction": direction, "nsid": "n",
				"kind": "answer", "tag": "ssh", "ciphertext": "-----BEGIN AGE",
			},
		}})
	}
	if in := event("inbound"); in.Contract != "c" || in.Ciphertext == "" || in.Kind != "answer" {
		t.Fatalf("inbound: %+v", in)
	}
	if out := event("outbound"); out.Ciphertext != "" {
		t.Fatalf("outbound ciphertext should stay out of the feed: %+v", out)
	}
}

func TestAgentProjection(t *testing.T) {
	if c := contract(&unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "c"}}}); c.Agent {
		t.Fatalf("no spec.agent: %+v", c)
	}
	c := contract(&unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "c"},
		"spec":     map[string]any{"agent": map[string]any{}},
	}})
	if !c.Agent {
		t.Fatalf("spec.agent {}: %+v", c)
	}
	e := contractEvent(&unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "e"},
		"status":   map[string]any{"phase": "Failed", "message": "no spec.agent"},
	}})
	if e.Phase != "Failed" || e.Message != "no spec.agent" {
		t.Fatalf("event status: %+v", e)
	}
}
