package exposure

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
)

func TestCurrentFollowsReregisteredHandler(t *testing.T) {
	b := bus.New(context.Background())
	c := &Controller{handlers: b}
	key := bus.Key{Workspace: "root", Kind: "Requester", Name: "requester"}
	answer := func(code int) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) })
	}
	serve := c.current(key, "")
	status := func() int {
		rec := httptest.NewRecorder()
		serve.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		return rec.Code
	}
	if got := status(); got != http.StatusServiceUnavailable {
		t.Fatalf("no handler: got %d, want 503", got)
	}
	if err := b.RegisterHTTP(key, answer(http.StatusTeapot)); err != nil {
		t.Fatal(err)
	}
	if got := status(); got != http.StatusTeapot {
		t.Fatalf("first handler: got %d, want 418", got)
	}
	if err := b.RegisterHTTP(key, answer(http.StatusAccepted)); err != nil {
		t.Fatal(err)
	}
	if got := status(); got != http.StatusAccepted {
		t.Fatalf("re-registered handler: got %d, want 202 (stale handler kept)", got)
	}
}
