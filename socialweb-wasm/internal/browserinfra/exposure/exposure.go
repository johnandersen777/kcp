// Package exposure is the browser-side substitute for the socialweb-computer-kcp
// Service/Exposure controllers and the exposure publisher. It opens no socket
// and terminates no TLS: an Exposure turns Ready with the configured public
// origin in status.address, and page-origin HTTP requests are dispatched
// in-process to the handler componentcore registered on the bus.
package exposure

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path"
	"strconv"
	"strings"

	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
)

// ConditionReady is the condition the real Exposure controller reports and the
// only condition this substitute writes.
const ConditionReady = "Ready"

// Origin is a validated public origin the browser serves the page from, for
// example https://abc.fedproxy.com. Host is the normalized authority used for
// routing; URL is the origin with a scheme.
type Origin struct {
	URL string

	Host string
}

func (o Origin) String() string { return o.URL }

// ParseOrigin validates a public origin. A bare host is treated as https.
// http is accepted only for localhost and loopback addresses, because the page
// itself is served over https everywhere else.
func ParseOrigin(raw string) (Origin, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Origin{}, errors.New("exposure: an origin is required")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Origin{}, fmt.Errorf("exposure: the origin %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return Origin{}, fmt.Errorf("exposure: the origin %q is not http or https", raw)
	}
	if u.User != nil {
		return Origin{}, fmt.Errorf("exposure: the origin %q has credentials", raw)
	}
	if u.Path != "" && u.Path != "/" {
		return Origin{}, fmt.Errorf("exposure: the origin %q has a path", raw)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return Origin{}, fmt.Errorf("exposure: the origin %q has a query or fragment", raw)
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" {
		return Origin{}, fmt.Errorf("exposure: the origin %q has no host", raw)
	}
	if u.Scheme == "http" && !loopback(host) {
		return Origin{}, fmt.Errorf("exposure: http is only allowed for localhost, not %q", host)
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return Origin{}, fmt.Errorf("exposure: the origin %q has an invalid port", raw)
		}
	}
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	authority := host
	if port != "" {
		authority = net.JoinHostPort(host, port)
	}
	return Origin{URL: u.Scheme + "://" + authority, Host: authority}, nil
}

func loopback(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// Status is the Ready Exposure status. Address is the field the app reads:
// internal/app/compose maps status.address to the endpoint URL and
// ApplyOAuthIdentityEndpoints uses it as the OAuth client origin.
//
// The Exposure API type has no status.observedGeneration or status.url field;
// the generation is carried on the Ready condition and the URL is
// status.address.
func Status(generation int64, origin, value string) exposurev1alpha1.ExposureStatus {
	address := Address(origin, value)
	st := exposurev1alpha1.ExposureStatus{
		Phase:    exposurev1alpha1.ExposureReady,
		Ready:    true,
		Address:  address,
		Endpoint: address,
	}
	conds.Set(&st.Conditions, generation, ConditionReady, true, "Ready", "the in-browser Exposure serves "+address)
	return st
}

// NotReady is the status while the Service or its handler is missing, or the
// Exposure is suspended or invalid. The real controller clears the address and
// endpoint the same way.
func NotReady(generation int64, phase exposurev1alpha1.ExposurePhase, reason, message string) exposurev1alpha1.ExposureStatus {
	st := exposurev1alpha1.ExposureStatus{Phase: phase}
	conds.Set(&st.Conditions, generation, ConditionReady, false, reason, message)
	return st
}

// Address joins the public origin with the Exposure path. An empty or "/" path
// yields the bare origin, matching the ingress publisher's originURL.
func Address(origin, value string) string {
	origin = strings.TrimRight(strings.TrimSpace(origin), "/")
	if p := normalizePath(value); p != "/" {
		return origin + p
	}
	return origin
}

func normalizePath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "/"
	}
	if !strings.HasPrefix(value, "/") {
		value = "/" + value
	}
	if value = path.Clean(value); value == "." {
		return "/"
	}
	return value
}
