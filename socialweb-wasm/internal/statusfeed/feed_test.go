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

func TestReleaseRequestProjection(t *testing.T) {
	c := contract(&unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "c"},
		"spec":     map[string]any{"release": true, "releaseRequestedAt": "2026-10-30T12:00:01.000Z"},
		"status": map[string]any{
			"phase":   "Failed",
			"release": map[string]any{"attempts": int64(3), "requestedAt": "2026-10-30T11:00:00.000Z"},
			"conditions": []any{
				map[string]any{"type": "Ready", "status": "False"},
				map[string]any{"type": "ReleaseFailed", "status": "True", "reason": "ReleaseBound"},
			},
		},
	}})
	if !c.Release || c.ReleaseRequestedAt != "2026-10-30T12:00:01.000Z" || c.ReleaseAnsweredAt != "2026-10-30T11:00:00.000Z" || !c.ReleaseFailed ||
		c.ReleaseFailedReason != "ReleaseBound" {
		t.Fatalf("release request: %+v", c)
	}
	plain := contract(&unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "c"},
		"status":   map[string]any{"conditions": []any{map[string]any{"type": "ReleaseFailed", "status": "False"}}},
	}})
	if plain.ReleaseRequestedAt != "" || plain.ReleaseAnsweredAt != "" || plain.ReleaseFailed || plain.ReleaseFailedReason != "" {
		t.Fatalf("no release: %+v", plain)
	}
}

func TestR2ReleaseProjection(t *testing.T) {
	with := func(rel map[string]any) Contract {
		return contract(&unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "c"},
			"status":   map[string]any{"contract": map[string]any{"r2Release": rel}},
		}})
	}
	waiting := with(map[string]any{"phase": "Requested", "requestedAt": "t1"})
	if waiting.R2Release == nil || waiting.R2Release.OK != nil || waiting.R2Release.RequestedAt != "t1" || waiting.R2Release.Phase != "Requested" {
		t.Fatalf("waiting: %+v", waiting.R2Release)
	}
	done := with(map[string]any{"phase": "Answered", "ok": false, "message": "timed out", "requestedAt": "t1", "answeredAt": "t2"})
	if done.R2Release.OK == nil || *done.R2Release.OK || done.R2Release.Message != "timed out" || done.R2Release.AnsweredAt != "t2" {
		t.Fatalf("answered: %+v", done.R2Release)
	}
	if c := contract(&unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "c"}}}); c.R2Release != nil {
		t.Fatalf("absent: %+v", c.R2Release)
	}
}

func TestGuestParts(t *testing.T) {
	with := func(c map[string]any) Contract {
		return contract(&unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "c"},
			"status":   map[string]any{"phase": "Failed", "contract": c},
		}})
	}
	receipt := map[string]any{"uri": "at://did:plc:b/x/1", "cid": "bafy"}
	type parts struct{ receiptOk, terminated, releaseOk bool }
	cases := []struct {
		name string
		c    map[string]any
		want parts
	}{
		{"ok receipt, no answer", map[string]any{"receipt": receipt, "receiptOk": true}, parts{true, false, false}},
		{"release answered not ok", map[string]any{"receipt": receipt, "receiptOk": true, "release": map[string]any{"ok": false}}, parts{true, false, false}},
		{"no receipt", map[string]any{"receiptOk": true}, parts{false, false, false}},
		{"receipt not ok", map[string]any{"receipt": receipt, "receiptOk": false}, parts{false, false, false}},
		{"receiptOk unset", map[string]any{"receipt": receipt}, parts{false, false, false}},
		{"terminated", map[string]any{"receipt": receipt, "receiptOk": true, "termination": map[string]any{"reason": "exit"}}, parts{true, true, false}},
		{"released ok", map[string]any{"receipt": receipt, "receiptOk": true, "release": map[string]any{"ok": true}}, parts{true, false, true}},
	}
	for _, tc := range cases {
		c := with(tc.c)
		if got := (parts{c.ReceiptOk, c.Terminated, c.ReleaseOk}); got != tc.want {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
	if c := contract(&unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "c"}}}); c.ReceiptOk || c.Terminated || c.ReleaseOk {
		t.Error("no status: nothing set")
	}
}

// AtircTokenMinted (socialweb r130) reaches the page as atircToken.
func TestContractAtircToken(t *testing.T) {
	withCond := func(status, reason, message string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "c"},
			"status": map[string]any{"conditions": []any{
				map[string]any{"type": "Ready", "status": "True"},
				map[string]any{"type": "AtircTokenMinted", "status": status, "reason": reason, "message": message},
			}},
		}}
	}
	if c := contract(&unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": "c"}}}); c.AtircToken != nil {
		t.Fatalf("no condition: atircToken = %+v, want nil (waiting for receipt)", c.AtircToken)
	}
	got := contract(withCond("True", "Leased", "the atirc agent token is in the bundle (Leased, attempt 1)")).AtircToken
	if got == nil || got.Status != "True" || got.Reason != "Leased" {
		t.Fatalf("leased: %+v", got)
	}
	got = contract(withCond("False", "MintFailed", "attempt 2 of 12: lease refused")).AtircToken
	if got == nil || got.Status != "False" || got.Reason != "MintFailed" || got.Message != "attempt 2 of 12: lease refused" {
		t.Fatalf("mint failed: %+v", got)
	}
}

func TestNamedPrefersThePagesOwnObject(t *testing.T) {
	obj := func(name, reason string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]any{"metadata": map[string]any{"name": name}, "status": map[string]any{"phase": "Ready"}}}
		if reason != "" {
			u.Object["status"].(map[string]any)["pendingAction"] = map[string]any{"kind": "signInAgain", "reason": reason}
		}
		return u
	}
	// "guest" sorts before "requester"; the page must show its own identity.
	items := []*unstructured.Unstructured{obj("requester", ""), obj("guest", "ScopeUpgrade")}
	if got := named(items, "requester", identityResource); got.Name != "requester" || got.Pending != nil {
		t.Fatalf("named = %+v, want requester without a pending action", got)
	}
	if got := named(items[1:], "requester", identityResource); got.Name != "guest" {
		t.Fatalf("named without requester = %+v, want the first by name", got)
	}
	if got := named(nil, "requester", identityResource); got != nil {
		t.Fatalf("named(nil) = %+v, want nil", got)
	}
}
