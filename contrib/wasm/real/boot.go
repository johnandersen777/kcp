/*
Copyright 2026 The KCP Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package real

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	cryptorand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/rest"

	"github.com/kcp-dev/kcp/contrib/wasm/storage"
	kcpmutatingadmissionpolicy "github.com/kcp-dev/kcp/pkg/admission/mutatingadmissionpolicy"
	kcpmutatingwebhook "github.com/kcp-dev/kcp/pkg/admission/mutatingwebhook"
	kcpvalidatingwebhook "github.com/kcp-dev/kcp/pkg/admission/validatingwebhook"
	"github.com/kcp-dev/kcp/pkg/server"
	serveroptions "github.com/kcp-dev/kcp/pkg/server/options"
)

// Config is the boot input for a real kcp control plane without sockets.
type Config struct {
	RootDir string

	// Address is the fake listener address used by kcp's own clients and
	// advertised URLs. It must be a loopback TCP address.
	Address string

	// ShardBaseURL is the virtual origin the browser uses, e.g.
	// http://kcp.local. It is advertised to clients.
	ShardBaseURL string

	// InitialSnapshot is the last combined storage snapshot, if any.
	InitialSnapshot []byte

	// OnSnapshot receives a combined snapshot after every mutation. It must
	// not block; the host persists it asynchronously.
	OnSnapshot func([]byte)

	// ReadyTimeout bounds the wait for /readyz.
	ReadyTimeout time.Duration
}

// Server is a running real kcp control plane.
type Server struct {
	KCP *server.Server

	Listener *PipeListener

	Dialer *PipeDialer

	// Client is the privileged loopback client. Its transport crosses the
	// in-process pipe and carries the loopback bearer token.
	Client *http.Client

	// BaseURL is the loopback base URL (https://127.0.0.1:6443).
	BaseURL string

	// VirtualOrigin is the browser-facing virtual origin.
	VirtualOrigin string

	dial   func(ctx context.Context, network, address string) (net.Conn, error)
	cancel context.CancelFunc
	done   chan struct{}
}

// Boot starts the real kcp server. It returns once the apiserver reports
// ready. The caller must keep the returned Server alive.
func Boot(parent context.Context, cfg Config) (*Server, error) {
	started := time.Now()
	stage := func(name string) {
		fmt.Fprintf(os.Stderr, "real: %s took %s\n", name, time.Since(started).Round(time.Millisecond))
	}
	if cfg.RootDir == "" {
		return nil, fmt.Errorf("real: RootDir is required")
	}
	if cfg.Address == "" {
		cfg.Address = "127.0.0.1:6443"
	}
	tcpAddr, err := net.ResolveTCPAddr("tcp", cfg.Address)
	if err != nil {
		return nil, fmt.Errorf("real: address %q: %w", cfg.Address, err)
	}
	if cfg.ReadyTimeout <= 0 {
		cfg.ReadyTimeout = 2 * time.Minute
	}

	listener := NewPipeListener(tcpAddr)
	dialer := NewPipeDialer(listener, 30*time.Second)
	// One func value for every client: the js build's client-go shares a
	// transport (and its HTTP/2 connection) per dial func value.
	dial := dialer.DialContext
	server.SetLoopbackDial(dial)

	// The logical-cluster migration controller needs raw etcd and is disabled
	// in memory mode. APIBindings check the migration annotation before it can
	// be written, so disable the feature in the browser shard too.
	_ = utilfeature.DefaultMutableFeatureGate.Set("LogicalClusterMigration=false")

	// NewOptions configures the cache server's separate memory store from this
	// environment variable. Set it before the options are built.
	if os.Getenv("KCP_MEMORY_STORAGE") == "" {
		_ = os.Setenv("KCP_MEMORY_STORAGE", "main")
	}
	// kcp generates a 4096-bit RSA service-account key on first boot, which
	// takes 3-12s in wasm (random prime search). The JWT signer accepts ECDSA
	// keys, so generate a P-256 key instead when none exists yet.
	if err := ensureServiceAccountKey(cfg.RootDir); err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("real: service-account key: %w", err)
	}

	opts := serveroptions.NewOptions(cfg.RootDir)
	opts.GenericControlPlane.Etcd.StorageConfig.Transport.ServerList = []string{"memory://main"}
	// The resource-quota admission evaluator walks informers that are not
	// ready during browser bootstrap and can block namespace creation and
	// /readyz for minutes. The browser shard does not need it. Webhooks
	// cannot be reached from the browser, and mutating admission policies are
	// unused; both wait for their informers on every write during bootstrap.
	// ValidatingAdmissionPolicy stays on: socialweb's tenant grants use it.
	opts.GenericControlPlane.Admission.GenericAdmission.DisablePlugins = append(
		opts.GenericControlPlane.Admission.GenericAdmission.DisablePlugins,
		"KCPKubeResourceQuota",
		kcpmutatingwebhook.PluginName,
		kcpvalidatingwebhook.PluginName,
		kcpmutatingadmissionpolicy.PluginName,
	)
	// Low-memory mode (iOS tabs): the apiserver watch cache keeps a second
	// decoded copy of every object plus event buffers per resource. The
	// memory:// store serves lists and watches (with history and initial
	// events) itself, so skip the cache.
	// A shorter event history pins fewer superseded object versions; watches
	// older than it relist.
	if os.Getenv("KCP_LOW_MEMORY") == "1" {
		opts.GenericControlPlane.Etcd.EnableWatchCache = false
		storage.SetWatchHistory(1024)
	}
	opts.GenericControlPlane.SecureServing.Listener = listener
	opts.GenericControlPlane.SecureServing.BindAddress = net.ParseIP("127.0.0.1")
	opts.GenericControlPlane.SecureServing.BindPort = tcpAddr.Port
	opts.GenericControlPlane.GenericServerRunOptions.AdvertiseAddress = net.ParseIP("127.0.0.1")
	opts.GenericControlPlane.SecureServing.ServerCert.CertDirectory = cfg.RootDir
	opts.Controllers.EnableLeaderElection = false
	if cfg.ShardBaseURL != "" {
		opts.Extra.ShardBaseURL = cfg.ShardBaseURL
	}
	// Keep the default batteries: the admin bootstrap creates the RBAC and
	// workspace state the rest of the controllers wait for. The files land in
	// the in-memory FS shim.

	completed, err := opts.Complete(parent, cfg.RootDir)
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("real: completing options: %w", err)
	}
	stage("options")
	if errs := completed.Validate(); len(errs) > 0 {
		_ = listener.Close()
		return nil, fmt.Errorf("real: validating options: %w", utilerrors.NewAggregate(errs))
	}

	serverConfig, err := server.NewConfig(parent, *completed)
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("real: building the server config: %w", err)
	}
	loopback := serverConfig.GenericConfig.LoopbackClientConfig
	loopback.Dial = dial
	if cfg.ShardBaseURL != "" {
		serverConfig.GenericConfig.ExternalAddress = hostOnly(cfg.ShardBaseURL)
	}
	completedConfig, err := serverConfig.Complete()
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("real: completing the server config: %w", err)
	}
	stage("config")
	kcpServer, err := server.NewServer(completedConfig)
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("real: building the server: %w", err)
	}
	stage("newserver")

	if cfg.InitialSnapshot != nil {
		kcpServer.PreRunHook = func(context.Context) error {
			return storage.RestoreAll(cfg.InitialSnapshot)
		}
	}
	if cfg.OnSnapshot != nil {
		storage.OnSnapshot(cfg.OnSnapshot)
	}

	transport, err := rest.TransportFor(loopback)
	if err != nil {
		_ = listener.Close()
		return nil, fmt.Errorf("real: loopback transport: %w", err)
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   0,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	baseURL := "https://" + cfg.Address

	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := kcpServer.Run(ctx); err != nil && ctx.Err() == nil {
			fmt.Printf("real: kcp exited: %v\n", err)
		}
	}()

	s := &Server{
		KCP:           kcpServer,
		Listener:      listener,
		Dialer:        dialer,
		dial:          dial,
		Client:        client,
		BaseURL:       baseURL,
		VirtualOrigin: cfg.ShardBaseURL,
		cancel:        cancel,
		done:          done,
	}
	if err := s.waitReady(ctx, cfg.ReadyTimeout); err != nil {
		_ = s.Close()
		return nil, err
	}
	stage("ready")
	return s, nil
}

func (s *Server) waitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("real: kcp did not become ready within %s", timeout)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.BaseURL+"/readyz", nil)
		if err != nil {
			return err
		}
		resp, err := s.Client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// Close stops the apiserver and the listener.
func (s *Server) Close() error {
	if s == nil {
		return nil
	}
	if s.cancel != nil {
		s.cancel()
	}
	// Let the apiserver close its own listener on shutdown. Closing it here
	// makes k8s.io/apiserver's RunServer panic on the closed-connection error.
	select {
	case <-s.done:
	case <-time.After(15 * time.Second):
		if s.Listener != nil {
			_ = s.Listener.Close()
		}
	}
	return nil
}

// RestConfig returns a copy of the privileged loopback client config.
func (s *Server) RestConfig() *rest.Config {
	if s == nil || s.KCP == nil {
		return nil
	}
	cfg := rest.CopyConfig(s.KCP.GenericConfig.LoopbackClientConfig)
	cfg.Dial = s.dial
	return cfg
}

// Kubeconfig returns a minimal kubeconfig for the privileged loopback client.
// The socialweb composer and some controllers still load their config from a
// file, so the browser writes this into its in-memory FS.
func (s *Server) Kubeconfig() []byte {
	if s == nil || s.KCP == nil {
		return nil
	}
	cfg := s.KCP.GenericConfig.LoopbackClientConfig
	ca := base64.StdEncoding.EncodeToString(cfg.CAData)
	if ca == "" && cfg.CAFile != "" {
		if raw, err := os.ReadFile(cfg.CAFile); err == nil {
			ca = base64.StdEncoding.EncodeToString(raw)
		}
	}
	server := cfg.Host
	if server == "" {
		server = s.BaseURL
	}
	return []byte(fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: kcp
  cluster:
    server: %s
    certificate-authority-data: %s
users:
- name: kcp
  user:
    token: %s
contexts:
- name: kcp
  context:
    cluster: kcp
    user: kcp
current-context: kcp
`, server, ca, cfg.BearerToken))
}

// TLSConfigForLoopback is a helper for callers that need to inspect the
// loopback TLS config (for tests).
func TLSConfigForLoopback(cfg *rest.Config) (*tls.Config, error) {
	if cfg == nil {
		return nil, fmt.Errorf("real: no rest config")
	}
	if cfg.TLSClientConfig.CAFile == "" && len(cfg.TLSClientConfig.CAData) == 0 {
		return nil, fmt.Errorf("real: loopback config has no CA")
	}
	return rest.TLSConfigFor(cfg)
}

// ensureServiceAccountKey writes an ECDSA P-256 service-account key when the
// root directory holds none. kcp's Controllers.Complete would otherwise
// generate an RSA-4096 key, which is very slow under wasm.
func ensureServiceAccountKey(rootDir string) error {
	path := filepath.Join(rootDir, "sa.key")
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), cryptorand.Reader)
	if err != nil {
		return err
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(rootDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600)
}

func hostOnly(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return u.Host
}
