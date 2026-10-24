package exposure

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	exposurev1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/exposure/v1alpha1"
)

func TestParseOrigin(t *testing.T) {
	ok := []struct {
		in   string
		url  string
		host string
	}{
		{"https://abc.fedproxy.com", "https://abc.fedproxy.com", "abc.fedproxy.com"},
		{"abc.fedproxy.com", "https://abc.fedproxy.com", "abc.fedproxy.com"},
		{"HTTPS://ABC.FedProxy.COM/", "https://abc.fedproxy.com", "abc.fedproxy.com"},
		{"https://abc.fedproxy.com:443", "https://abc.fedproxy.com", "abc.fedproxy.com"},
		{"http://localhost:8090", "http://localhost:8090", "localhost:8090"},
		{"http://127.0.0.1:8080", "http://127.0.0.1:8080", "127.0.0.1:8080"},
		{"http://[::1]:8080", "http://[::1]:8080", "[::1]:8080"},
		{"https://localhost:8090", "https://localhost:8090", "localhost:8090"},
	}
	for _, tc := range ok {
		origin, err := ParseOrigin(tc.in)
		if err != nil {
			t.Fatalf("ParseOrigin(%q): %v", tc.in, err)
		}
		if origin.URL != tc.url || origin.Host != tc.host {
			t.Fatalf("ParseOrigin(%q) = %#v, want url %q host %q", tc.in, origin, tc.url, tc.host)
		}
	}
	bad := []string{
		"",
		"http://abc.fedproxy.com",
		"http://0.0.0.0:8080",
		"ftp://abc.fedproxy.com",
		"https://user:pass@abc.fedproxy.com",
		"https://abc.fedproxy.com/path",
		"https://abc.fedproxy.com?q=1",
		"https://abc.fedproxy.com#frag",
		"https://",
		"https://abc.fedproxy.com:notaport",
	}
	for _, in := range bad {
		if origin, err := ParseOrigin(in); err == nil {
			t.Fatalf("ParseOrigin(%q) = %#v, want an error", in, origin)
		}
	}
}

func TestStatus(t *testing.T) {
	st := Status(7, "https://abc.fedproxy.com", "/")
	if st.Phase != exposurev1alpha1.ExposureReady || !st.Ready {
		t.Fatalf("status = %#v, want Ready", st)
	}
	if st.Address != "https://abc.fedproxy.com" || st.Endpoint != "https://abc.fedproxy.com" {
		t.Fatalf("address/endpoint = %q/%q", st.Address, st.Endpoint)
	}
	cond := meta.FindStatusCondition(st.Conditions, ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.ObservedGeneration != 7 {
		t.Fatalf("Ready condition = %#v", cond)
	}

	st = Status(3, "https://abc.fedproxy.com/", "xrpc/com.example")
	if st.Address != "https://abc.fedproxy.com/xrpc/com.example" {
		t.Fatalf("address = %q, want the origin plus the cleaned path", st.Address)
	}

	notReady := NotReady(4, exposurev1alpha1.ExposurePending, "ServiceMissing", "no service")
	if notReady.Phase != exposurev1alpha1.ExposurePending || notReady.Ready || notReady.Address != "" || notReady.Endpoint != "" {
		t.Fatalf("not-ready status = %#v", notReady)
	}
	cond = meta.FindStatusCondition(notReady.Conditions, ConditionReady)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "ServiceMissing" || cond.ObservedGeneration != 4 {
		t.Fatalf("Ready condition = %#v", cond)
	}
}

func TestRegistryDispatchAndCleanup(t *testing.T) {
	registry := NewRegistry()
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	if err := registry.Register("ws/requester", "abc.fedproxy.com", "/", handler); err != nil {
		t.Fatal(err)
	}
	got, ok := registry.Lookup("https://abc.fedproxy.com/xrpc/com.example", "")
	if !ok {
		t.Fatal("Lookup missed the registered route")
	}
	rec := httptest.NewRecorder()
	got.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "https://abc.fedproxy.com/xrpc/com.example", nil))
	if rec.Body.String() != "ok" {
		t.Fatalf("body = %q", rec.Body.String())
	}
	registry.Remove("ws/requester")
	if _, ok := registry.Lookup("abc.fedproxy.com", "/"); ok {
		t.Fatal("Lookup found a removed route")
	}
	registry.Remove("ws/requester") // idempotent

	if err := registry.Register("", "host", "/", handler); err == nil {
		t.Fatal("Register accepted an empty owner")
	}
	if err := registry.Register("ws/x", "", "/", handler); err == nil {
		t.Fatal("Register accepted an empty host")
	}
	if err := registry.Register("ws/x", "host", "/", nil); err == nil {
		t.Fatal("Register accepted a nil handler")
	}
}

func TestRegistryHostAndPathRouting(t *testing.T) {
	registry := NewRegistry()
	root := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	oauth := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	api := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	register := func(owner, host, path string, h http.Handler) {
		t.Helper()
		if err := registry.Register(owner, host, path, h); err != nil {
			t.Fatal(err)
		}
	}
	register("a/root", "https://Example.COM:443", "/", root)
	register("a/oauth", "example.com", "/oauth", oauth)
	register("a/api", "example.com", "api", api)
	register("b/port", "localhost:8090", "/", root)
	register("c/other", "other.example.com", "/", root)

	cases := []struct {
		host, path string
		want       http.Handler
	}{
		{"example.com", "/", root},
		{"EXAMPLE.com:443", "/anything", root},
		{"https://example.com", "/oauth/callback/requester", oauth},
		{"example.com", "/oauth", oauth},
		{"example.com", "/apix", root}, // no segment leak from /api
		{"example.com", "/api/v1", api},
		{"localhost:8090", "/", root},
		{"localhost:9090", "/", nil},
		{"other.example.com", "/x", root},
		{"", "/", nil},
	}
	for _, tc := range cases {
		got, ok := registry.HandlerFor(tc.host, tc.path)
		if tc.want == nil {
			if ok {
				t.Fatalf("HandlerFor(%q, %q) found a route", tc.host, tc.path)
			}
			continue
		}
		if !ok || got == nil {
			t.Fatalf("HandlerFor(%q, %q) missed the route", tc.host, tc.path)
		}
	}

	rec := httptest.NewRecorder()
	registry.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "https://example.com/nope", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("ServeHTTP status = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	registry.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "https://unknown.example/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("ServeHTTP for an unknown host = %d, want 404", rec.Code)
	}
}

func TestRegistryReplaceAndConcurrency(t *testing.T) {
	registry := NewRegistry()
	first := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	second := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	if err := registry.Register("ws/x", "host", "/", first); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register("ws/x", "host", "/", second); err != nil {
		t.Fatal(err)
	}
	if got, _ := registry.Lookup("host", "/"); got == nil {
		t.Fatal("replaced route is missing")
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = registry.Register("ws/x", "host", "/", first)
		}()
		go func() {
			defer wg.Done()
			_, _ = registry.Lookup("host", "/")
		}()
	}
	wg.Wait()
}

func TestNormalizePath(t *testing.T) {
	cases := map[string]string{"": "/", "/": "/", "x": "/x", "/x/": "/x", "//x//y": "/x/y", "/../x": "/x"}
	for in, want := range cases {
		if got := normalizePath(in); got != want {
			t.Fatalf("normalizePath(%q) = %q, want %q", in, got, want)
		}
	}
	if got := Address("https://host/", "/"); got != "https://host" {
		t.Fatalf("Address = %q", got)
	}
	if got := strings.TrimSuffix(Address("https://host", "api"), "/"); got != "https://host/api" {
		t.Fatalf("Address = %q", got)
	}
}
