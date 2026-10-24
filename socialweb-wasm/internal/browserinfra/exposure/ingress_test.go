package exposure

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bluesky-social/indigo/atproto/atcrypto"

	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/ingress"

	bvault "github.com/johnandersen777/socialweb-computer-kcp/wasm/internal/browserinfra/vault"
)

func TestIngressIdentityAndServe(t *testing.T) {
	ctx := context.Background()
	store := bvault.NewStore()
	var published []Ingress
	opts := Options{
		Origin:           "http://localhost:8090",
		IngressProxyHost: "xrpc.fedproxy.com",
		Keys:             func(string) *bvault.Store { return store },
		OnIngress:        func(list []Ingress) { published = list },
	}
	c, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	spec := exposurev1alpha1.ExposureSpec{Mode: exposurev1alpha1.ExposureModeIngress}
	in, err := c.ingress.register(ctx, "root/svc-requester-requester-ingress", "root", "svc-requester-requester-ingress", spec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(in.DID, "did:key:z") {
		t.Fatalf("did = %q", in.DID)
	}
	wantHost := strings.ToLower(strings.ReplaceAll(in.DID, ":", "-")) + ".xrpc.fedproxy.com"
	if in.Host != wantHost || in.URL != "https://"+wantHost || in.ProxyHost != "xrpc.fedproxy.com" {
		t.Fatalf("ingress = %#v", in)
	}
	if len(published) != 1 || published[0].Host != in.Host {
		t.Fatalf("published = %#v", published)
	}

	// The key persists in the vault store: a fresh controller (a reload with
	// the restored snapshot) gets the same did:key.
	again, _ := New(opts)
	in2, err := again.ingress.register(ctx, "root/svc-requester-requester-ingress", "root", "svc-requester-requester-ingress", spec)
	if err != nil || in2.DID != in.DID {
		t.Fatalf("reloaded did = %q, %v; want %q", in2.DID, err, in.DID)
	}

	// The nonce signature verifies against the did:key.
	nonce := []byte("nonce-bytes")
	sig, err := c.SignIngress(in.Host, nonce)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := atcrypto.ParsePublicDIDKey(in.DID)
	if err != nil {
		t.Fatal(err)
	}
	if err := pub.HashAndVerify(nonce, sig); err != nil {
		t.Fatalf("signature: %v", err)
	}
	token, err := c.IngressServiceAuth(in.Host, ingress.SubscribeNSID)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := ingress.VerifyServiceAuth(ctx, token, "did:web:xrpc.fedproxy.com", ingress.SubscribeNSID, nil)
	if err != nil || auth.Issuer != in.DID {
		t.Fatalf("service auth = %#v, %v", auth, err)
	}
	if _, err := c.SignIngress("localhost:8090", nonce); err == nil {
		t.Fatal("signed for a host that is not an ingress Exposure")
	}

	// Requests reach only the ingress Exposure's routes on its host.
	var seen *http.Request
	var seenBody string
	_ = c.registry.Register("root/svc-requester-requester-ingress", in.Host, "/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r
		b, _ := io.ReadAll(r.Body)
		seenBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("WWW-Authenticate", "Bearer")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"AuthenticationRequired"}`))
	}))
	_ = c.registry.Register("root/page", "localhost:8090", "/private", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("a page-origin route answered a relay request")
	}))
	res := c.ServeIngress(ctx, IngressRequest{
		Host: in.Host, Method: "POST", Path: "/xrpc/x.y.z", Params: map[string]string{"a": "1"},
		Headers: map[string]string{"authorization": "Bearer t"}, RawBody: `{"k":"v"}`,
	})
	if res.Status != http.StatusUnauthorized || res.Body != `{"error":"AuthenticationRequired"}` ||
		res.ContentType != "application/json" || res.Headers["www-authenticate"] != "Bearer" {
		t.Fatalf("response = %#v", res)
	}
	if seen.URL.RawQuery != "a=1" || seen.Host != in.Host || seen.Header.Get("Authorization") != "Bearer t" ||
		seen.Header.Get("X-Forwarded-Proto") != "https" || seenBody != `{"k":"v"}` {
		t.Fatalf("request = %#v body %q", seen, seenBody)
	}
	if res := c.ServeIngress(ctx, IngressRequest{Host: "localhost:8090", Path: "/private"}); res.Status != http.StatusNotFound {
		t.Fatalf("page-origin host via relay = %#v", res)
	}

	c.remove("root/svc-requester-requester-ingress")
	if len(published) != 0 || len(c.Ingresses()) != 0 {
		t.Fatalf("after remove: published %#v", published)
	}
	if res := c.ServeIngress(ctx, IngressRequest{Host: in.Host, Path: "/"}); res.Status != http.StatusNotFound {
		t.Fatalf("removed ingress still served: %#v", res)
	}
}

func TestIngressOff(t *testing.T) {
	c, err := New(Options{Origin: "http://localhost:8090"})
	if err != nil {
		t.Fatal(err)
	}
	if c.ingress != nil || c.Ingresses() != nil {
		t.Fatal("ingress mode is on without a relay host")
	}
	if res := c.ServeIngress(context.Background(), IngressRequest{Host: "x", Path: "/"}); res.Status != http.StatusNotFound {
		t.Fatalf("ServeIngress = %#v", res)
	}
}
