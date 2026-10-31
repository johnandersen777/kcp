// Package kcpios boots the real kcp control plane inside an iOS app.
//
// It is the native counterpart of socialweb-wasm/cmd/kcp-wasm: the same
// memory:// storage backend and net.Pipe listener, without syscall/js. All
// calls are safe from any goroutine; Start blocks until /readyz passes.
package kcpios

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/pkg/embedded"
	"github.com/kcp-dev/kcp/contrib/wasm/real"
	"github.com/kcp-dev/kcp/contrib/wasm/storage"
)

const (
	address      = "127.0.0.1:6443"
	shardBaseURL = "https://kcp.local"
)

var (
	mu              sync.Mutex
	srv             *real.Server
	cancel          context.CancelFunc
	rootDir         string
	requester       *embedded.Requester
	requesterCancel context.CancelFunc
)

// Version reports the Go runtime version, proving the Go runtime is live.
func Version() string { return runtime.Version() }

type status struct {
	Ready     bool   `json:"ready"`
	BaseURL   string `json:"baseURL,omitempty"`
	GoVersion string `json:"goVersion"`
}

// Start boots kcp under rootDir. A non-empty snapshot restores prior state.
// It returns a JSON status document once the apiserver is ready.
func Start(dir, snapshot string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return "", fmt.Errorf("kcpios: root dir is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("kcpios: root dir: %w", err)
	}

	mu.Lock()
	running := srv != nil
	if !running {
		rootDir = dir
	}
	mu.Unlock()
	if running {
		return statusJSON()
	}

	var initial []byte
	if snapshot != "" {
		raw, err := base64.StdEncoding.DecodeString(snapshot)
		if err != nil {
			return "", fmt.Errorf("kcpios: snapshot: %w", err)
		}
		if err := storage.CheckSnapshot(raw); err != nil {
			return "", fmt.Errorf("kcpios: saved state is unreadable: %w", err)
		}
		initial = raw
	}

	// The cache server's separate store and low-memory mode are selected
	// through the environment before kcp's options are built. iOS jetsam
	// kills the app long before a browser tab's memory limit is reached, so
	// skip the apiserver watch cache and shorten the watch history.
	_ = os.Setenv("KCP_MEMORY_STORAGE", "main")
	_ = os.Setenv("KCP_LOW_MEMORY", "1")

	ctx, cancelFn := context.WithCancel(context.Background())
	// Boot with two cores left for the UI: Go threads ignore Swift QoS and
	// would otherwise take every core while kcp warms up. Restore the default
	// parallelism once the control plane is ready.
	procs := runtime.NumCPU() - 2
	if procs < 1 {
		procs = 1
	}
	previousProcs := runtime.GOMAXPROCS(procs)
	defer runtime.GOMAXPROCS(previousProcs)
	booted, err := real.Boot(ctx, real.Config{
		RootDir:         dir,
		Address:         address,
		ShardBaseURL:    shardBaseURL,
		InitialSnapshot: initial,
		ReadyTimeout:    3 * time.Minute,
	})
	if err != nil {
		cancelFn()
		return "", fmt.Errorf("kcpios: boot: %w", err)
	}

	mu.Lock()
	srv, cancel = booted, cancelFn
	mu.Unlock()
	// Collect harder once boot is done: iOS jetsam kills the app long before a
	// browser tab's wasm limit. Boot keeps the default GOGC for speed. The soft
	// memory limit keeps the heap from growing into jetsam territory; raise it
	// if the requester graph needs more headroom on newer devices.
	debug.SetGCPercent(25)
	debug.SetMemoryLimit(1 << 30)
	return statusJSON()
}

// Ready reports whether kcp has booted in this process.
func Ready() bool {
	mu.Lock()
	defer mu.Unlock()
	return srv != nil
}

// StatusJSON reports readiness as JSON.
func StatusJSON() string {
	out, _ := statusJSON()
	return out
}

func statusJSON() (string, error) {
	mu.Lock()
	defer mu.Unlock()
	st := status{Ready: srv != nil, GoVersion: runtime.Version()}
	if srv != nil {
		st.BaseURL = srv.BaseURL
	}
	raw, err := json.Marshal(st)
	return string(raw), err
}

// StartRequester starts the socialweb market requester in-process, on top of
// the running kcp. configJSON carries the signed-in identity (handle, DID, PDS
// URL and OAuth client fields); see the Swift bridge.
func StartRequester(configJSON string) (string, error) {
	mu.Lock()
	server, dir := srv, rootDir
	running := requester != nil
	mu.Unlock()
	if server == nil {
		return "", fmt.Errorf("kcpios: kcp is not running")
	}
	if running {
		return requesterStatusJSON()
	}

	type config struct {
		Handle           string `json:"handle"`
		DID              string `json:"did"`
		PDSURL           string `json:"pdsUrl"`
		OAuthClientID    string `json:"oauthClientId"`
		OAuthRedirectURI string `json:"oauthRedirectUri"`
		StateDir         string `json:"stateDir"`
		Origin           string `json:"origin"`
		Restore          string `json:"restore"`
		ForceApply       bool   `json:"forceApply"`
	}
	var cfg config
	if configJSON != "" {
		if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
			return "", fmt.Errorf("kcpios: requester config: %w", err)
		}
	}
	if cfg.StateDir == "" {
		cfg.StateDir = filepath.Join(dir, "socialweb")
	}
	var restore []byte
	if cfg.Restore != "" {
		raw, err := base64.StdEncoding.DecodeString(cfg.Restore)
		if err != nil {
			return "", fmt.Errorf("kcpios: requester snapshot: %w", err)
		}
		restore = raw
	}

	ctx, cancelFn := context.WithCancel(context.Background())
	r, err := embedded.Start(ctx, embedded.Config{
		Rest:             server.RestConfig(),
		StateDir:         cfg.StateDir,
		Origin:           cfg.Origin,
		Handle:           cfg.Handle,
		DID:              cfg.DID,
		PDSURL:           cfg.PDSURL,
		OAuthClientID:    cfg.OAuthClientID,
		OAuthRedirectURI: cfg.OAuthRedirectURI,
		ForceApply:       cfg.ForceApply,
		Restore:          restore,
	})
	if err != nil {
		cancelFn()
		return "", fmt.Errorf("kcpios: requester: %w", err)
	}

	mu.Lock()
	requester, requesterCancel = r, cancelFn
	mu.Unlock()
	return requesterStatusJSON()
}

// RequesterReady reports whether the socialweb requester is running.
func RequesterReady() bool {
	mu.Lock()
	defer mu.Unlock()
	return requester != nil
}

func requesterStatusJSON() (string, error) {
	raw, err := json.Marshal(map[string]bool{"ready": RequesterReady()})
	return string(raw), err
}

// RequesterSnapshot returns the requester's state snapshot, base64 encoded.
func RequesterSnapshot() (string, error) {
	mu.Lock()
	r := requester
	mu.Unlock()
	if r == nil {
		return "", fmt.Errorf("kcpios: requester is not running")
	}
	raw, err := r.Snapshot()
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// Request performs one HTTPS request against the loopback apiserver and
// returns a JSON response document. headersJSON is a JSON object of header
// name to value; body is sent verbatim.
func Request(method, path, headersJSON, body string) (string, error) {
	mu.Lock()
	server := srv
	mu.Unlock()
	if server == nil {
		return "", fmt.Errorf("kcpios: kcp is not running")
	}
	if method == "" {
		method = http.MethodGet
	}
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	// The socialweb requester owns the OAuth callback and the exposure/ingress
	// paths it composes. Try it first, with the un-rewritten path.
	if rq := currentRequester(); rq != nil {
		if out, handled, err := serveRequester(rq, method, path, headersJSON, body, server); handled {
			return out, err
		}
	}
	// The shard apiserver requires a logical cluster on every workspace
	// request. Clients here use the virtual origin, so default them to root.
	// Virtual-workspace service URLs are resolved by host and path and must
	// not be rewritten.
	systemPath := path == "/version" || path == "/readyz" || path == "/healthz" || path == "/livez"
	if !systemPath &&
		!strings.HasPrefix(path, "/clusters/") && !strings.HasPrefix(path, "/services/") {
		path = "/clusters/root" + path
	}

	req, err := http.NewRequest(method, server.BaseURL+path, strings.NewReader(body))
	if err != nil {
		return "", err
	}
	if headersJSON != "" {
		var headers map[string]string
		if err := json.Unmarshal([]byte(headersJSON), &headers); err != nil {
			return "", fmt.Errorf("kcpios: headers: %w", err)
		}
		for key, value := range headers {
			if strings.EqualFold(key, "host") {
				req.Host = value
				continue
			}
			if strings.EqualFold(key, "content-length") {
				continue
			}
			req.Header.Set(key, value)
		}
	}
	if req.Header.Get("Authorization") == "" {
		if token := loopbackToken(server); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	// The privileged loopback user bypasses CreatedBy admission. Impersonate a
	// non-privileged kcp admin so Workspaces get spec.createdBy and APIBindings
	// can initialize. System paths stay on the loopback identity: /version and
	// the health endpoints are not granted to impersonated users.
	if !systemPath && req.Header.Get("Impersonate-User") == "" {
		req.Header.Set("Impersonate-User", "system:kcp:admin")
		req.Header.Set("Impersonate-Group", "system:kcp:admin")
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}

	resp, err := server.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("kcpios: request: %w", err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 0, 32*1024)
	tmp := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if readErr != nil {
			break
		}
	}

	out := map[string]any{"status": resp.StatusCode, "headers": resp.Header}
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotModified && len(buf) > 0 {
		out["bodyBase64"] = base64.StdEncoding.EncodeToString(buf)
		if json.Valid(buf) {
			out["bodyJSON"] = json.RawMessage(buf)
		}
	}
	raw, err := json.Marshal(out)
	return string(raw), err
}

func currentRequester() *embedded.Requester {
	mu.Lock()
	defer mu.Unlock()
	return requester
}

func encodeResponse(status int, headers http.Header, body []byte) (string, error) {
	out := map[string]any{"status": status, "headers": headers}
	if status != http.StatusNoContent && status != http.StatusNotModified && len(body) > 0 {
		out["bodyBase64"] = base64.StdEncoding.EncodeToString(body)
		if json.Valid(body) {
			out["bodyJSON"] = json.RawMessage(body)
		}
	}
	raw, err := json.Marshal(out)
	return string(raw), err
}

// serveRequester answers the OAuth callback and the requester's virtual
// exposure paths. It reports handled=false for every other path.
func serveRequester(rq *embedded.Requester, method, path, headersJSON, body string, server *real.Server) (string, bool, error) {
	req, err := http.NewRequest(method, server.BaseURL+path, strings.NewReader(body))
	if err != nil {
		return "", true, err
	}
	if headersJSON != "" {
		var headers map[string]string
		if err := json.Unmarshal([]byte(headersJSON), &headers); err != nil {
			return "", true, fmt.Errorf("kcpios: headers: %w", err)
		}
		for key, value := range headers {
			if strings.EqualFold(key, "host") {
				req.Host = value
				continue
			}
			if strings.EqualFold(key, "content-length") {
				continue
			}
			req.Header.Set(key, value)
		}
	}
	if strings.HasPrefix(path, "/oauth/callback/requester") {
		if err := rq.CompleteOAuth(context.Background(), req); err != nil {
			out, encErr := encodeResponse(http.StatusBadRequest, nil, []byte(err.Error()))
			return out, true, encErr
		}
		out, err := encodeResponse(http.StatusOK, nil, []byte("signed in"))
		return out, true, err
	}
	host := req.Host
	if host == "" {
		host = "kcp.local"
	}
	handler, ok := rq.Lookup(host, path)
	if !ok {
		return "", false, nil
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	out, err := encodeResponse(rec.Code, rec.Header(), rec.Body.Bytes())
	return out, true, err
}

// Snapshot returns the combined memory:// state, base64 encoded, for the host
// to persist. It is empty when kcp has not booted.
func Snapshot() (string, error) {
	mu.Lock()
	running := srv != nil
	mu.Unlock()
	if !running {
		return "", fmt.Errorf("kcpios: kcp is not running")
	}
	raw, err := storage.SnapshotAll()
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}

// Stop shuts the apiserver down and clears process state.
func Stop() {
	mu.Lock()
	server, cancelFn := srv, cancel
	rq, reqCancel := requester, requesterCancel
	srv, cancel = nil, nil
	requester, requesterCancel = nil, nil
	mu.Unlock()
	if rq != nil {
		_ = rq.Close()
	}
	if reqCancel != nil {
		reqCancel()
	}
	if cancelFn != nil {
		cancelFn()
	}
	if server != nil {
		_ = server.Close()
	}
}

func loopbackToken(server *real.Server) string {
	if server == nil || server.KCP == nil {
		return ""
	}
	cfg := server.KCP.GenericConfig.LoopbackClientConfig
	if cfg == nil {
		return ""
	}
	return cfg.BearerToken
}
