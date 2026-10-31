package atircgo

import "github.com/kcp-dev/kcp/contrib/wasm/ios/kcpios"

// KCPStart boots the in-process kcp control plane. See kcpios.Start.
func KCPStart(rootDir, snapshot string) (string, error) {
	return kcpios.Start(rootDir, snapshot)
}

// KCPRequest performs one loopback apiserver request. See kcpios.Request.
func KCPRequest(method, path, headersJSON, body string) (string, error) {
	return kcpios.Request(method, path, headersJSON, body)
}

// KCPSnapshot returns the base64 memory:// state. See kcpios.Snapshot.
func KCPSnapshot() (string, error) {
	return kcpios.Snapshot()
}

// KCPReady reports whether kcp has booted.
func KCPReady() bool {
	return kcpios.Ready()
}

// KCPStatus reports readiness as JSON.
func KCPStatus() string {
	return kcpios.StatusJSON()
}

// KCPStop shuts kcp down.
func KCPStop() {
	kcpios.Stop()
}

// PolicySandbox is the host half of the fenced policy sandbox. The iOS app
// implements it with a WKWebView and passes it to KCPRequesterStart.
type PolicySandbox interface {
	Run(requestJSON []byte) ([]byte, error)
	Kill(runID string)
	PlantProbe(marker string) ([]byte, error)
	ClearProbe(marker string) error
}

// KCPRequesterStart starts the socialweb market requester on the running kcp.
func KCPRequesterStart(configJSON string, sandbox PolicySandbox) (string, error) {
	return kcpios.StartRequester(configJSON, sandbox)
}

// KCPPutContractSecrets writes a contract's secrets bundle before the
// ComputeContract is created.
func KCPPutContractSecrets(contract string, bundleJSON []byte) error {
	return kcpios.PutContractSecrets(contract, bundleJSON)
}

// KCPSecrets is the class-method form of PutContractSecrets: gomobile's
// generated classes import reliably in the app's explicit-module Xcode build.
type KCPSecrets struct{}

// NewKCPSecrets returns a KCPSecrets.
func NewKCPSecrets() *KCPSecrets { return &KCPSecrets{} }

// Put writes the contract's secrets bundle.
func (k *KCPSecrets) Put(contract string, bundleJSON []byte) error {
	return kcpios.PutContractSecrets(contract, bundleJSON)
}

// KCPRequesterReady reports whether the requester is running.
func KCPRequesterReady() bool {
	return kcpios.RequesterReady()
}

// KCPRequesterSnapshot returns the requester state, base64 encoded.
func KCPRequesterSnapshot() (string, error) {
	return kcpios.RequesterSnapshot()
}
