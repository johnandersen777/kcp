//go:build js && wasm

// kcp-wasm boots the real kcp control plane over an in-process net.Pipe and
// exposes its HTTP surface to the browser worker. The socialweb requester
// graph is linked in here; later milestones register its orchestrator.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"syscall/js"
	"time"
	"unicode/utf8"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/wasm/internal/browserinfra/exposure"
	"github.com/johnandersen777/socialweb-computer-kcp/wasm/internal/socialweb"
	"github.com/johnandersen777/socialweb-computer-kcp/wasm/internal/statusfeed"
	"github.com/kcp-dev/kcp/contrib/wasm/real"
)

type request struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
}

type response struct {
	Status     int               `json:"status"`
	Headers    map[string]string `json:"headers,omitempty"`
	Body       string            `json:"body,omitempty"`
	BodyBase64 string            `json:"bodyBase64,omitempty"`
}

func main() {
	// Allocation profiling samples callers through the wasm pclntab; with this
	// module size the js runtime occasionally reports an invalid symbol table
	// and aborts. The browser has no use for heap profiles.
	runtime.MemProfileRate = 0

	rootDir := "/kcp"
	cfg := real.Config{
		RootDir:       rootDir,
		Address:       "127.0.0.1:6443",
		ShardBaseURL:  "https://kcp.local",
		ReadyTimeout:  3 * time.Minute,
	}
	if initial := js.Global().Get("__kcpInitialState"); initial.Type() == js.TypeString {
		if raw := initial.String(); raw != "" {
			cfg.InitialSnapshot = []byte(raw)
		}
	}
	cfg.OnSnapshot = func(snapshot []byte) {
		persist := js.Global().Get("__kcpPersist")
		if persist.Type() == js.TypeFunction {
			persist.Invoke(string(snapshot))
		}
	}

	srv, err := real.Boot(context.Background(), cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kcp-wasm: boot:", err)
		js.Global().Set("kcpHandleRequest", js.FuncOf(func(js.Value, []js.Value) any {
			return nil
		}))
		js.Global().Set("__kcpBootError", err.Error())
		select {}
	}

	handle := js.FuncOf(func(_ js.Value, args []js.Value) any {
		if len(args) < 2 {
			return nil
		}
		raw := args[0].String()
		id := args[1].Int()
		go func() {
			out := handleRequest(srv, raw)
			js.Global().Call("__kcpResolve", id, out)
		}()
		return nil
	})
	js.Global().Set("kcpHandleRequest", handle)
	js.Global().Set("__kcpWasmReady", true)
	fmt.Fprintln(os.Stderr, "kcp-wasm: real kcp ready at", srv.BaseURL)

	// The socialweb control plane starts after kcp is ready. It is optional:
	// the kcp API is usable while the app composes.
	go startSocialweb(srv, handle)

	select {}
}

func startSocialweb(srv *real.Server, kcpHandler js.Func) {
	defer func() {
		if value := recover(); value != nil {
			js.Global().Set("__socialwebError", fmt.Sprint(value))
		}
	}()
	cfg := socialweb.Config{
		Rest:         srv.RestConfig(),
		Bus:          bus.NewHandlerRegistry(context.Background()),
		StateDir:     "/kcp/socialweb",
		Origin:       "http://127.0.0.1:8090",
		IdentityMode: v1alpha1.MarketIdentityModeOAuthBrowser,
	}
	statusFeed := true
	cfg.IngressProxyHost = defaultIngressProxyHost
	if raw := js.Global().Get("__socialwebConfig"); raw.Type() == js.TypeString && raw.String() != "" {
		var in struct {
			// IngressProxyHost overrides the relay; "" serves every Exposure
			// on the page origin.
			IngressProxyHost *string `json:"ingressProxyHost"`
			Origin           string   `json:"origin"`
			Handle           string   `json:"handle"`
			DID              string   `json:"did"`
			PDSURL           string   `json:"pdsUrl"`
			OAuthClientID    string   `json:"oauthClientId"`
			OAuthRedirectURI string   `json:"oauthRedirectUri"`
			IdentityMode     string   `json:"identityMode"`
			BidderDIDs       []string `json:"bidderDids"`
			ForceApply       bool     `json:"forceApply"`
			Restore          string   `json:"restore"`
			StatusFeed       *bool    `json:"statusFeed"`
		}
		if err := json.Unmarshal([]byte(raw.String()), &in); err == nil {
			cfg.Origin = firstNonEmpty(in.Origin, cfg.Origin)
			cfg.Handle = in.Handle
			cfg.DID = in.DID
			cfg.PDSURL = in.PDSURL
			cfg.OAuthClientID = in.OAuthClientID
			cfg.OAuthRedirectURI = in.OAuthRedirectURI
			cfg.BidderDIDs = in.BidderDIDs
			cfg.ForceApply = in.ForceApply
			if restore := strings.TrimSpace(in.Restore); restore != "" {
				cfg.Restore = json.RawMessage(restore)
			}
			if in.StatusFeed != nil {
				statusFeed = *in.StatusFeed
			}
			if in.IngressProxyHost != nil {
				cfg.IngressProxyHost = strings.TrimSpace(*in.IngressProxyHost)
			}
			if in.IdentityMode != "" {
				cfg.IdentityMode = v1alpha1.MarketIdentityMode(in.IdentityMode)
			}
		}
	}
	cfg.OnSnapshot = func(snapshot []byte) {
		persist := js.Global().Get("__socialwebPersist")
		if persist.Type() == js.TypeFunction {
			persist.Invoke(string(snapshot))
		}
	}
	cfg.OnIngress = func(list []exposure.Ingress) {
		if fn := js.Global().Get("__socialwebIngress"); fn.Type() == js.TypeFunction {
			raw, _ := json.Marshal(list)
			fn.Invoke(string(raw))
		}
	}
	orc, err := socialweb.Start(context.Background(), cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "kcp-wasm: socialweb:", err)
		js.Global().Set("__socialwebError", err.Error())
		return
	}
	registerPageExports(orc)
	if statusFeed {
		go func() {
			err := statusfeed.Start(context.Background(), srv.RestConfig(), func(raw []byte) {
				js.Global().Call("__kcpState", string(raw))
			})
			if err != nil {
				fmt.Fprintln(os.Stderr, "kcp-wasm: status feed:", err)
			}
		}()
	}
	handler := js.FuncOf(func(_ js.Value, args []js.Value) any {
		if len(args) < 2 {
			return nil
		}
		raw := args[0].String()
		id := args[1].Int()
		go func() {
			var req request
			if err := json.Unmarshal([]byte(raw), &req); err != nil {
				js.Global().Call("__kcpResolve", id, encode(response{Status: http.StatusBadRequest, Body: err.Error()}))
				return
			}
			if strings.HasPrefix(req.Path, "/oauth/callback/requester") {
				rec := httptest.NewRecorder()
				httpReq, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
					"http://"+firstNonEmpty(req.Headers["Host"], "kcp.local")+req.Path, nil)
				if err == nil {
					err = orc.CompleteRequesterOAuth(context.Background(), httpReq)
				}
				if err != nil {
					rec.WriteHeader(http.StatusBadRequest)
					_, _ = rec.WriteString(err.Error())
				} else {
					rec.WriteHeader(http.StatusOK)
					_, _ = rec.WriteString("signed in")
				}
				js.Global().Call("__kcpResolve", id, encode(response{
					Status:  rec.Code,
					Headers: map[string]string{"content-type": "text/plain; charset=utf-8"},
					Body:    rec.Body.String(),
				}))
				return
			}
			if h, ok := orc.Lookup(req.Headers["Host"], req.Path); ok {
				rec := httptest.NewRecorder()
				httpReq, err := http.NewRequest(req.Method, "http://"+firstNonEmpty(req.Headers["Host"], "kcp.local")+req.Path, strings.NewReader(req.Body))
				if err != nil {
					js.Global().Call("__kcpResolve", id, encode(response{Status: http.StatusBadRequest, Body: err.Error()}))
					return
				}
				for k, v := range req.Headers {
					if strings.EqualFold(k, "Host") {
						httpReq.Host = v
						continue
					}
					httpReq.Header.Set(k, v)
				}
				h.ServeHTTP(rec, httpReq)
				out := response{Status: rec.Code, Headers: map[string]string{}}
				for k, v := range rec.Header() {
					if len(v) > 0 {
						out.Headers[k] = v[0]
					}
				}
				if utf8Printable(rec.Body.Bytes()) {
					out.Body = rec.Body.String()
				} else {
					out.BodyBase64 = base64.StdEncoding.EncodeToString(rec.Body.Bytes())
				}
				js.Global().Call("__kcpResolve", id, encode(out))
				return
			}
			js.Global().Call("__kcpResolve", id, handleRequest(srv, raw))
		}()
		return nil
	})
	js.Global().Set("socialwebHandleRequest", handler)
	js.Global().Set("__socialwebReady", true)
	fmt.Fprintln(os.Stderr, "kcp-wasm: socialweb ready")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func handleRequest(srv *real.Server, raw string) string {
	var req request
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		return encode(response{Status: http.StatusBadRequest, Body: "invalid request: " + err.Error()})
	}
	if req.Path == "" {
		req.Path = "/"
	}
	path := req.Path
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	// kcp's shard apiserver requires a logical cluster on every workspace
	// request. Browser clients use the virtual origin without one; default
	// them to root.
	// Virtual-workspace service URLs (/services/apiexport/...) are resolved by
	// host and path and must not be rewritten.
	if !strings.HasPrefix(path, "/clusters/") && !strings.HasPrefix(path, "/services/") &&
		path != "/version" && path != "/readyz" && path != "/healthz" && path != "/livez" {
		path = "/clusters/root" + path
	}
	httpReq, err := http.NewRequest(req.Method, srv.BaseURL+path, strings.NewReader(req.Body))
	if err != nil {
		return encode(response{Status: http.StatusBadRequest, Body: err.Error()})
	}
	for key, value := range req.Headers {
		if strings.EqualFold(key, "host") {
			httpReq.Host = value
			continue
		}
		if strings.EqualFold(key, "content-length") {
			continue
		}
		httpReq.Header.Set(key, value)
	}
	if httpReq.Header.Get("Authorization") == "" {
		if token := loopbackToken(srv); token != "" {
			httpReq.Header.Set("Authorization", "Bearer "+token)
		}
	}
	// The privileged loopback user bypasses CreatedBy admission. Impersonate a
	// non-privileged kcp admin so Workspaces get spec.createdBy and APIBindings
	// can initialize.
	if httpReq.Header.Get("Impersonate-User") == "" {
		httpReq.Header.Set("Impersonate-User", "system:kcp:admin")
		httpReq.Header.Set("Impersonate-Group", "system:kcp:admin")
	}
	if httpReq.Header.Get("Accept") == "" {
		httpReq.Header.Set("Accept", "application/json")
	}
	resp, err := srv.Client.Do(httpReq)
	if err != nil {
		return encode(response{Status: http.StatusBadGateway, Body: "kcp: " + err.Error()})
	}
	defer resp.Body.Close()
	buf := make([]byte, 0, 32*1024)
	tmp := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if readErr != nil {
			break
		}
	}
	out := response{Status: resp.StatusCode, Headers: map[string]string{}}
	for key, values := range resp.Header {
		if len(values) > 0 {
			out.Headers[key] = values[0]
		}
	}
	// Fetch forbids a body on null-body statuses; kubectl's discovery cache
	// produces 304s.
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotModified && len(buf) > 0 {
		if utf8Printable(buf) {
			out.Body = string(buf)
		} else {
			out.BodyBase64 = base64.StdEncoding.EncodeToString(buf)
		}
	}
	return encode(out)
}

func loopbackToken(srv *real.Server) string {
	if srv == nil || srv.KCP == nil {
		return ""
	}
	return srv.KCP.GenericConfig.LoopbackClientConfig.BearerToken
}

func utf8Printable(raw []byte) bool { return utf8.Valid(raw) }

func encode(v response) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}
