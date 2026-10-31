// Command kcpsmoke boots kcp with the iOS entrypoint and queries /version.
// It is the host-side smoke test for the same package the iOS app links.
package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"time"

	"github.com/kcp-dev/kcp/contrib/wasm/ios/kcpios"
)

func main() {
	root, err := os.MkdirTemp("", "kcpios-smoke-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "smoke: temp dir:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(root)

	status, err := kcpios.Start(root, "")
	if err != nil {
		fmt.Fprintln(os.Stderr, "smoke: start:", err)
		os.Exit(1)
	}
	fmt.Println("smoke: status:", status)

	version, err := kcpios.Request("GET", "/version", "", "")
	if err != nil {
		fmt.Fprintln(os.Stderr, "smoke: version:", err)
		os.Exit(1)
	}
	body, _ := base64.StdEncoding.DecodeString(extractBody(version))
	fmt.Println("smoke: /version:", string(body))

	apis, err := kcpios.Request("GET", "/apis", "", "")
	if err != nil {
		fmt.Fprintln(os.Stderr, "smoke: apis:", err)
		os.Exit(1)
	}
	apisBody, _ := base64.StdEncoding.DecodeString(extractBody(apis))
	if len(apisBody) > 160 {
		apisBody = apisBody[:160]
	}
	fmt.Println("smoke: /apis:", string(apisBody), "...")

	if _, err := kcpios.StartRequester("{}"); err != nil {
		fmt.Fprintln(os.Stderr, "smoke: requester start:", err)
		os.Exit(1)
	}
	time.Sleep(20 * time.Second)
	group, err := kcpios.Request("GET", "/apis/socialweb.computer", "", "")
	if err != nil {
		fmt.Fprintln(os.Stderr, "smoke: group:", err)
		os.Exit(1)
	}
	groupBody, _ := base64.StdEncoding.DecodeString(extractBody(group))
	if len(groupBody) > 200 {
		groupBody = groupBody[:200]
	}
	fmt.Println("smoke: socialweb group:", string(groupBody))

	for _, path := range []string{
		"/apis/tenancy.kcp.io/v1alpha1/workspaces?limit=50",
		"/apis/apis.kcp.io/v1alpha1/apibindings",
		"/apis/socialweb.computer/v1alpha1",
		"/apis/socialweb.computer/v1alpha1/marketidentities/requester",
		"/apis/socialweb.computer/v1alpha1/marketidentities",
		"/apis/socialweb.computer/v1alpha1/marketrequesters",
		"/apis/apis.kcp.io/v1alpha1/apibindings?limit=50",
		"/clusters/root:socialweb-provider/apis",
		"/clusters/root:socialweb-provider/apis/socialweb.computer/v1alpha1",
		"/clusters/root:socialweb-provider/apis/socialweb.computer/v1alpha1/marketidentities/requester",
	} {
		out, err := kcpios.Request("GET", path, "", "")
		if err != nil {
			fmt.Fprintf(os.Stderr, "smoke: %s: %v\n", path, err)
			continue
		}
		body, _ := base64.StdEncoding.DecodeString(extractBody(out))
		if len(body) > 300 {
			body = body[:300]
		}
		fmt.Printf("smoke: %s -> %s\n", path, string(body))
	}
	kcpios.Stop()
}

// extractBody pulls bodyBase64 out of the response JSON without a full decode
// dependency in the smoke tool.
func extractBody(raw string) string {
	const key = `"bodyBase64":"`
	start := -1
	for i := 0; i+len(key) <= len(raw); i++ {
		if raw[i:i+len(key)] == key {
			start = i + len(key)
			break
		}
	}
	if start < 0 {
		return ""
	}
	end := start
	for end < len(raw) && raw[end] != '"' {
		end++
	}
	return raw[start:end]
}
