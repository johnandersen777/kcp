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

// KCPRequesterStart starts the socialweb market requester on the running kcp.
func KCPRequesterStart(configJSON string) (string, error) {
	return kcpios.StartRequester(configJSON)
}

// KCPRequesterReady reports whether the requester is running.
func KCPRequesterReady() bool {
	return kcpios.RequesterReady()
}

// KCPRequesterSnapshot returns the requester state, base64 encoded.
func KCPRequesterSnapshot() (string, error) {
	return kcpios.RequesterSnapshot()
}
