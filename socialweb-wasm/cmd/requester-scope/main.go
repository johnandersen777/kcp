// Command requester-scope prints the OAuth scope the in-browser requester asks
// for, so the page's client metadata is generated from socialweb's own
// constant and cannot drift from it.
package main

import (
	"fmt"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauth"
)

func main() {
	fmt.Println(oauth.ScopeString(oauth.RequesterOAuthScope))
}
