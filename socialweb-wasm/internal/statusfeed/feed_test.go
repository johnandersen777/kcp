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

func TestAssociationProjection(t *testing.T) {
	c := contract(&unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "c"},
		"status": map[string]any{"association": map[string]any{"uri": "at://did:plc:me/computer.socialweb.market.association/k", "cid": "bafy"},
			"associationMessage": "subject refused"},
	}})
	if c.Association != "at://did:plc:me/computer.socialweb.market.association/k" || c.AssociationMessage != "subject refused" {
		t.Fatalf("status.association: %+v", c)
	}
}

func TestGuestRequesterProjection(t *testing.T) {
	if c := contract(&unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "c"}}}); c.GuestRequester != nil {
		t.Fatalf("no guestRequester: %+v", c)
	}
	c := contract(&unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "c"},
		"status": map[string]any{"contract": map[string]any{"guestRequester": map[string]any{
			"did": "did:plc:guest", "state": "Verified", "message": "ok", "reportedAt": "2026-10-30T00:00:00Z"}}},
	}})
	if c.GuestRequester == nil || *c.GuestRequester != (GuestRequester{DID: "did:plc:guest", State: "Verified", Message: "ok"}) {
		t.Fatalf("guestRequester: %+v", c.GuestRequester)
	}
}

func TestR2Projection(t *testing.T) {
	c := contract(&unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "c"},
		"spec":     map[string]any{"guestTools": []any{"socialweb-computer"}, "association": map[string]any{"fromGuest": true}},
	}})
	if len(c.GuestTools) != 1 || c.GuestTools[0] != "socialweb-computer" || !c.FromGuest {
		t.Fatalf("R2 spec: %+v", c)
	}
	if c := contract(&unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "c"}}}); c.GuestTools != nil || c.FromGuest {
		t.Fatalf("no R2: %+v", c)
	}
}

func TestReleaseCascadeProjection(t *testing.T) {
	c := contract(&unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "c"},
		"status": map[string]any{"phase": "Releasing", "atircRevokes": "Pending",
			"execution": map[string]any{"step": "releasingGuestRequester"}},
	}})
	if c.Step != "releasingGuestRequester" || c.AtircRevokes != "Pending" {
		t.Fatalf("release cascade: %+v", c)
	}
}
