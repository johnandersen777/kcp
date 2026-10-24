package exposure

import (
	"cmp"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"

	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/ingress"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/hostport"

	bvault "github.com/johnandersen777/socialweb-computer-kcp/wasm/internal/browserinfra/vault"
)

// Ingress mode is the browser substitute for internal/exposure's
// SubscriberRegistrar (provider xrpc-subscription). Each ingress Exposure gets
// its own K-256 did:key, as componentcore.ExposureIngressIdentity does, and
// status.address becomes https://<did label>.<relay host>. Go cannot open a
// WebSocket here, so the page holds the subscription
// (scripts/browser/ingress_ws.js) and calls SignIngress, IngressServiceAuth and
// ServeIngress through the worker.

const (
	// IdentityMount is the browser vault mount holding the per-Exposure keys,
	// so they persist with the socialweb snapshot. No component Vault scope
	// names it.
	IdentityMount = "exposure-identity"

	// ServiceAuthTTL matches componentcore.ExposureServiceAuthTTL.
	ServiceAuthTTL = 5 * time.Minute

	relayMaxBody = 8 << 20
)

// Ingress is one registered ingress Exposure the page must subscribe for.
type Ingress struct {
	Workspace string `json:"workspace"`
	Name      string `json:"name"`
	DID       string `json:"did"`
	// ProxyHost is the relay the page connects to, e.g. xrpc.fedproxy.com.
	ProxyHost string `json:"proxyHost"`
	// Host is the public did-key host the relay forwards for this Exposure.
	Host string `json:"host"`
	URL  string `json:"url"`
}

type ingressEntry struct {
	Ingress
	key *atcrypto.PrivateKeyK256
}

type ingressState struct {
	proxyHost string
	keys      func(workspace string) *bvault.Store
	notify    func([]Ingress)

	mu      sync.Mutex
	entries map[string]ingressEntry // owner -> entry
}

// key loads or creates the did:key for one Exposure.
func (s *ingressState) key(ctx context.Context, workspace, name string) (*atcrypto.PrivateKeyK256, error) {
	if s.keys == nil {
		return nil, errors.New("exposure: ingress mode has no key store")
	}
	store := s.keys(workspace)
	data, err := store.Read(ctx, IdentityMount, name, 0)
	switch {
	case err == nil:
		raw, _ := data["k256"].(string)
		b, err := hex.DecodeString(raw)
		if err != nil {
			return nil, fmt.Errorf("exposure: the ingress key for %s/%s: %w", workspace, name, err)
		}
		return atcrypto.ParsePrivateBytesK256(b)
	case !errors.Is(err, bvault.ErrNotFound):
		return nil, err
	}
	priv, err := atcrypto.GeneratePrivateKeyK256()
	if err != nil {
		return nil, err
	}
	if err := store.Write(ctx, IdentityMount, name, map[string]any{"k256": hex.EncodeToString(priv.Bytes())}); err != nil {
		return nil, err
	}
	return priv, nil
}

// register resolves the identity and records the Exposure as served through
// the relay. It mirrors SubscriberRegistrar.Register's host and URL rules.
func (s *ingressState) register(ctx context.Context, owner, workspace, name string, spec exposurev1alpha1.ExposureSpec) (Ingress, error) {
	proxyHost := cmp.Or(strings.TrimSpace(spec.IngressAddress), s.proxyHost)
	s.mu.Lock()
	if existing, ok := s.entries[owner]; ok && existing.ProxyHost == proxyHost {
		s.mu.Unlock()
		return existing.Ingress, nil
	}
	s.mu.Unlock()
	priv, err := s.key(ctx, workspace, name)
	if err != nil {
		return Ingress{}, err
	}
	pub, err := priv.PublicKey()
	if err != nil {
		return Ingress{}, err
	}
	did := pub.DIDKey()
	publicHost := cmp.Or(strings.TrimSpace(spec.PublicHost), hostport.URLHostname(proxyHost))
	host := hostport.DIDLabel(did) + "." + hostport.HostnameOnly(publicHost)
	in := Ingress{Workspace: workspace, Name: name, DID: did, ProxyHost: proxyHost, Host: host, URL: "https://" + host}
	s.mu.Lock()
	s.entries[owner] = ingressEntry{Ingress: in, key: priv}
	list := s.listLocked()
	s.mu.Unlock()
	s.publish(list)
	return in, nil
}

func (s *ingressState) drop(owner string) {
	s.mu.Lock()
	if _, ok := s.entries[owner]; !ok {
		s.mu.Unlock()
		return
	}
	delete(s.entries, owner)
	list := s.listLocked()
	s.mu.Unlock()
	s.publish(list)
}

func (s *ingressState) publish(list []Ingress) {
	if s.notify != nil {
		s.notify(list)
	}
}

func (s *ingressState) listLocked() []Ingress {
	out := make([]Ingress, 0, len(s.entries))
	for _, e := range s.entries {
		out = append(out, e.Ingress)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Host < out[j].Host })
	return out
}

func (s *ingressState) byHost(host string) (ingressEntry, bool) {
	host = normalizeHost(host)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.entries {
		if e.Host == host {
			return e, true
		}
	}
	return ingressEntry{}, false
}

// Ingresses lists the Exposures served through the relay.
func (c *Controller) Ingresses() []Ingress {
	if c.ingress == nil {
		return nil
	}
	c.ingress.mu.Lock()
	defer c.ingress.mu.Unlock()
	return c.ingress.listLocked()
}

// SignIngress signs the relay registration nonce with the did:key of the
// Exposure served on host, as ingress.Subscriber does with its Keypair.
func (c *Controller) SignIngress(host string, nonce []byte) ([]byte, error) {
	e, err := c.ingressEntry(host)
	if err != nil {
		return nil, err
	}
	return e.key.HashAndSign(nonce)
}

// IngressServiceAuth mints the service-auth JWT the relay checks on the nonce
// and subscribe calls: iss is the Exposure did:key, aud did:web:<relay host>.
func (c *Controller) IngressServiceAuth(host, nsid string) (string, error) {
	e, err := c.ingressEntry(host)
	if err != nil {
		return "", err
	}
	aud := "did:web:" + hostport.URLHostname(e.ProxyHost)
	return ingress.SignServiceAuth(e.key, e.DID, aud, nsid, ServiceAuthTTL)
}

func (c *Controller) ingressEntry(host string) (ingressEntry, error) {
	if c.ingress == nil {
		return ingressEntry{}, errors.New("exposure: ingress mode is off")
	}
	e, ok := c.ingress.byHost(host)
	if !ok {
		return ingressEntry{}, fmt.Errorf("exposure: no ingress Exposure serves %q", host)
	}
	return e, nil
}

// IngressRequest is a relay request frame as the page forwards it.
type IngressRequest struct {
	Host    string            `json:"host"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Params  map[string]string `json:"params,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    json.RawMessage   `json:"body,omitempty"`
	RawBody string            `json:"rawBody,omitempty"`
}

// IngressResponse is the response frame payload.
type IngressResponse struct {
	Status      int               `json:"status"`
	ContentType string            `json:"contentType,omitempty"`
	Body        string            `json:"body"`
	Headers     map[string]string `json:"headers,omitempty"`
}

// ServeIngress answers one relay request. Only the routes of ingress
// Exposures registered for the host are reachable: never page-origin routes
// and never the kcp API. Request and response mapping mirrors
// SubscriberRegistrar.handler.
func (c *Controller) ServeIngress(ctx context.Context, in IngressRequest) IngressResponse {
	fail := func(status int, code, message string) IngressResponse {
		raw, _ := json.Marshal(map[string]string{"error": code, "message": message})
		return IngressResponse{Status: status, ContentType: "application/json", Body: string(raw)}
	}
	e, err := c.ingressEntry(in.Host)
	if err != nil {
		return fail(http.StatusNotFound, "NotFound", err.Error())
	}
	path := cmp.Or(in.Path, "/")
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	handler, ok := c.registry.Lookup(e.Host, path)
	if !ok {
		return fail(http.StatusNotFound, "NotFound", "no route for "+path)
	}
	if len(in.Params) > 0 {
		values := url.Values{}
		for key, value := range in.Params {
			values.Set(key, value)
		}
		path += "?" + values.Encode()
	}
	body := in.RawBody
	if body == "" && len(in.Body) > 0 && string(in.Body) != "null" {
		body = string(in.Body)
	}
	req, err := http.NewRequestWithContext(ctx, cmp.Or(in.Method, http.MethodGet), "https://"+e.Host+path, strings.NewReader(body))
	if err != nil {
		return fail(http.StatusBadRequest, "InvalidRequest", err.Error())
	}
	for key, value := range in.Headers {
		req.Header.Set(key, value)
	}
	req.Host = e.Host
	if req.Header.Get("X-Forwarded-Host") == "" {
		req.Header.Set("X-Forwarded-Host", req.Host)
	}
	if req.Header.Get("X-Forwarded-Proto") == "" {
		req.Header.Set("X-Forwarded-Proto", "https")
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	res := rec.Result()
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, relayMaxBody))
	out := IngressResponse{Status: res.StatusCode, ContentType: res.Header.Get("Content-Type"), Body: string(raw), Headers: map[string]string{}}
	for key := range res.Header {
		out.Headers[strings.ToLower(key)] = res.Header.Get(key)
	}
	return out
}
