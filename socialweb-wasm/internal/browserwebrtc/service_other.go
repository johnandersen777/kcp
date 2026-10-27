//go:build !js || !wasm

package browserwebrtc

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/tunnel"
)

type Config struct {
	Handler http.Handler

	Log *slog.Logger

	Options tunnel.ServiceOptions
}

func NewService(cfg Config) (tunnel.Service, error) {
	return nil, errors.New("browserwebrtc: the browser WebRTC bridge needs js/wasm")
}
