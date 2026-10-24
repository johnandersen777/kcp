package socialweb

import (
	"net/http"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"
)

// BrowserDirectory resolves AT Protocol handles and DIDs with fetch() only.
//
// indigo's DefaultDirectory installs an SSRF dialer, which makes net/http use
// the socket stack instead of fetch() under GOOS=js and fails with "dial tcp:
// lookup ...". The wasm runtime has no sockets. An empty DNS suffix skips DNS
// entirely (every string has the empty suffix), so handles resolve through the
// public HTTPS well-known endpoint, and did:plc/did:web through fetch as well.
func BrowserDirectory() identity.Directory {
	client := &http.Client{Timeout: 10 * time.Second}
	return &identity.BaseDirectory{
		PLCURL:                identity.DefaultPLCURL,
		HTTPClient:            *client,
		PLCClient:             client,
		SkipDNSDomainSuffixes: []string{""},
	}
}
