//go:build js && wasm

package browserwebrtc

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"syscall/js"
	"time"

	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/tunnel"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/channelconn"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/httptunnel"
)

const (
	OfferTimeout = 20 * time.Second

	RequestDeadline = 60 * time.Second

	DefaultSessionTTL = 10 * time.Minute

	DefaultMaxSessions = 64
)

type pendingOffer struct {
	offer tunnel.Offer

	timer *time.Timer

	accepted bool
}

type Config struct {
	Handler http.Handler

	Log *slog.Logger

	Options tunnel.ServiceOptions
}

type Service struct {
	handler http.Handler

	log *slog.Logger

	ctx context.Context

	cancel context.CancelFunc

	mu sync.Mutex

	// ponytail: every answered offer keeps its RTCPeerConnection so a late bid
	// can use any live one; cap per peer if a long-lived tab accumulates them.
	pending map[string]*pendingOffer

	channels map[string]*channelconn.Conn

	options tunnel.ServiceOptions

	closed bool
}

var (
	registry sync.Mutex

	current *Service

	installed sync.Once
)

func NewService(cfg Config) (*Service, error) {
	if cfg.Handler == nil {
		return nil, errors.New("browserwebrtc: an ingress needs an HTTP handler")
	}
	bridge := js.Global().Get("__socialwebWebRTC")
	if bridge.Type() != js.TypeObject || bridge.Get("offer").Type() != js.TypeFunction {
		return nil, errors.New("browserwebrtc: the page has no WebRTC bridge")
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	service := &Service{
		handler:  cfg.Handler,
		log:      log,
		ctx:      ctx,
		cancel:   cancel,
		pending:  map[string]*pendingOffer{},
		channels: map[string]*channelconn.Conn{},
	}
	install()
	options := cfg.Options
	if len(options.STUN) == 0 {
		options.STUN = []string{tunnel.DefaultSTUN}
	}
	if options.SessionTTL <= 0 {
		options.SessionTTL = DefaultSessionTTL
	}
	if options.MaxSessions <= 0 {
		options.MaxSessions = DefaultMaxSessions
	}
	service.options = options
	if setter := bridge.Get("setIceServers"); setter.Type() == js.TypeFunction {
		urls := js.Global().Get("Array").New(len(options.STUN))
		for i, url := range options.STUN {
			urls.SetIndex(i, url)
		}
		setter.Invoke(urls)
	}
	registry.Lock()
	current = service
	registry.Unlock()
	return service, nil
}

func active() *Service {
	registry.Lock()
	defer registry.Unlock()
	return current
}

func install() {
	installed.Do(func() {
		js.Global().Set("__socialwebWebRTCOpen", js.FuncOf(onOpen))
		js.Global().Set("__socialwebWebRTCData", js.FuncOf(onData))
		js.Global().Set("__socialwebWebRTCClose", js.FuncOf(onClose))
		js.Global().Set("__socialwebWebRTCState", js.FuncOf(onState))
	})
}

func onOpen(_ js.Value, args []js.Value) any {
	if len(args) < 2 {
		return nil
	}
	if service := active(); service != nil {
		service.open(args[0].String(), args[1].String())
	}
	return nil
}

func onData(_ js.Value, args []js.Value) any {
	if len(args) < 3 {
		return nil
	}
	service := active()
	if service == nil {
		return nil
	}
	raw, err := base64.StdEncoding.DecodeString(args[2].String())
	if err != nil {
		service.log.Warn("browserwebrtc_chunk_invalid", "err", err)
		return nil
	}
	service.mu.Lock()
	conn := service.channels[channelKey(args[0].String(), args[1].String())]
	service.mu.Unlock()
	if conn != nil {
		conn.Push(raw)
	}
	return nil
}

func onClose(_ js.Value, args []js.Value) any {
	if len(args) < 2 {
		return nil
	}
	service := active()
	if service == nil {
		return nil
	}
	service.mu.Lock()
	conn := service.channels[channelKey(args[0].String(), args[1].String())]
	service.mu.Unlock()
	if conn != nil {
		conn.CloseWithError(io.EOF)
	}
	return nil
}

func onState(_ js.Value, args []js.Value) any {
	service := active()
	if service == nil || len(args) == 0 {
		return nil
	}
	session := args[0].String()
	state := ""
	if len(args) > 1 {
		state = args[1].String()
	}
	detail := ""
	if len(args) > 2 {
		detail = args[2].String()
	}
	service.log.Info("browserwebrtc_state", "session", session, "state", state, "detail", detail)
	if state == "connected" {
		service.mu.Lock()
		if pending := service.pending[session]; pending != nil {
			pending.accepted = true
			if pending.timer != nil {
				pending.timer.Stop()
			}
		}
		service.mu.Unlock()
	}
	if state == "failed" || state == "closed" {
		service.mu.Lock()
		if pending := service.pending[session]; pending != nil {
			if pending.timer != nil {
				pending.timer.Stop()
			}
			delete(service.pending, session)
		}
		service.mu.Unlock()
	}
	return nil
}

func (s *Service) Offer(ctx context.Context, owner, peer string) (tunnel.Offer, error) {
	if s == nil {
		return tunnel.Offer{}, errors.New("browserwebrtc: the ingress is not running")
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return tunnel.Offer{}, errors.New("browserwebrtc: the ingress is closed")
	}
	id := tunnel.ID(owner)
	if id == "" {
		return tunnel.Offer{}, errors.New("browserwebrtc: the offer names no requester identity")
	}
	if strings.TrimSpace(peer) == "" {
		return tunnel.Offer{}, errors.New("browserwebrtc: the offer names no peer")
	}
	session := tunnel.NewSession()
	if session == "" {
		return tunnel.Offer{}, errors.New("browserwebrtc: the offer session could not be generated")
	}
	s.mu.Lock()
	full := s.pendingLocked() >= s.options.MaxSessions
	s.mu.Unlock()
	if full {
		return tunnel.Offer{}, fmt.Errorf("browserwebrtc: the ingress holds %d pending sessions", s.options.MaxSessions)
	}
	sdp, err := s.callOffer(ctx, session, peer)
	if err != nil {
		return tunnel.Offer{}, err
	}
	offer := tunnel.Offer{Kind: tunnel.KindWebRTC, ID: id, Session: session, Peer: peer, Type: tunnel.SignalOffer, SDP: sdp}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.closeSession(session)
		return tunnel.Offer{}, errors.New("browserwebrtc: the ingress is closed")
	}
	if s.pendingLocked() >= s.options.MaxSessions {
		s.mu.Unlock()
		s.closeSession(session)
		return tunnel.Offer{}, fmt.Errorf("browserwebrtc: the ingress holds %d pending sessions", s.options.MaxSessions)
	}
	state := &pendingOffer{offer: offer}
	if s.options.SessionTTL > 0 {
		state.timer = time.AfterFunc(s.options.SessionTTL, func() {
			s.expire(session)
		})
	}
	s.pending[session] = state
	s.mu.Unlock()
	s.log.Info("browserwebrtc_offer", "peer", peer, "session", session, "sdpBytes", len(sdp))
	return offer, nil
}

func (s *Service) Accept(ctx context.Context, answer tunnel.Answer) error {
	if s == nil {
		return errors.New("browserwebrtc: the ingress is not running")
	}
	if !tunnel.ValidAnswer(answer) {
		return errors.New("browserwebrtc: the answer is not a webrtc answer")
	}
	s.mu.Lock()
	state, ok := s.pending[answer.Session]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("browserwebrtc: no offer names session %s", answer.Session)
	}
	if state.offer.ID != answer.ID {
		return fmt.Errorf("browserwebrtc: the answer names ingress %s and the offer %s", answer.ID, state.offer.ID)
	}
	if state.offer.Peer != answer.Peer {
		return fmt.Errorf("browserwebrtc: the answer names peer %s and the offer %s", answer.Peer, state.offer.Peer)
	}
	js.Global().Get("__socialwebWebRTC").Call("accept", answer.Session, answer.SDP)
	s.log.Info("browserwebrtc_answer", "peer", answer.Peer, "session", answer.Session)
	return nil
}

func (s *Service) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	sessions := make([]string, 0, len(s.pending))
	for session, pending := range s.pending {
		if pending.timer != nil {
			pending.timer.Stop()
		}
		sessions = append(sessions, session)
	}
	channels := s.channels
	s.pending = map[string]*pendingOffer{}
	s.channels = map[string]*channelconn.Conn{}
	s.mu.Unlock()
	s.cancel()
	for _, session := range sessions {
		s.closeSession(session)
	}
	for _, conn := range channels {
		conn.CloseWithError(context.Canceled)
	}
	return nil
}

func (s *Service) open(session, id string) {
	key := channelKey(session, id)
	send := func(data []byte) error {
		js.Global().Get("__socialwebWebRTC").Call("send", session, id, base64.StdEncoding.EncodeToString(data))
		return nil
	}
	shut := func() error {
		js.Global().Get("__socialwebWebRTC").Call("closeChannel", session, id)
		return nil
	}
	conn := channelconn.New(send, shut)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = conn.Close()
		return
	}
	s.channels[key] = conn
	s.mu.Unlock()
	stop := context.AfterFunc(s.ctx, func() {
		conn.CloseWithError(context.Canceled)
	})
	go func() {
		defer stop()
		defer s.drop(key)
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(RequestDeadline))
		if err := httptunnel.Serve(s.ctx, conn, s.handler); err != nil && s.ctx.Err() == nil && !errors.Is(err, io.EOF) {
			s.log.Warn("browserwebrtc_request_failed", "session", session, "err", err)
		}
	}()
}

func (s *Service) drop(key string) {
	s.mu.Lock()
	delete(s.channels, key)
	s.mu.Unlock()
}

func (s *Service) pendingLocked() int {
	count := 0
	for _, pending := range s.pending {
		if !pending.accepted {
			count++
		}
	}
	return count
}

func (s *Service) expire(session string) {
	s.mu.Lock()
	pending := s.pending[session]
	if pending == nil || pending.accepted {
		s.mu.Unlock()
		return
	}
	delete(s.pending, session)
	s.mu.Unlock()
	s.closeSession(session)
}

func (s *Service) closeSession(session string) {
	if session == "" {
		return
	}
	js.Global().Get("__socialwebWebRTC").Call("close", session)
}

func (s *Service) callOffer(ctx context.Context, session, peer string) (string, error) {
	type result struct {
		sdp string
		err error
	}
	done := make(chan result, 1)
	var callback js.Func
	callback = js.FuncOf(func(_ js.Value, args []js.Value) any {
		defer callback.Release()
		if len(args) == 0 {
			done <- result{err: errors.New("browserwebrtc: the browser returned no offer")}
			return nil
		}
		if first := args[0]; first.Type() != js.TypeNull && first.Type() != js.TypeUndefined {
			done <- result{err: errors.New("browserwebrtc: " + first.String())}
			return nil
		}
		if len(args) < 2 || args[1].Type() != js.TypeObject {
			done <- result{err: errors.New("browserwebrtc: the browser returned no offer")}
			return nil
		}
		done <- result{sdp: args[1].Get("sdp").String()}
		return nil
	})
	js.Global().Get("__socialwebWebRTC").Call("offer", session, peer, callback)
	timer := time.NewTimer(OfferTimeout)
	defer timer.Stop()
	select {
	case out := <-done:
		return out.sdp, out.err
	case <-ctx.Done():
		return "", ctx.Err()
	case <-timer.C:
		return "", fmt.Errorf("browserwebrtc: the browser did not answer within %s", OfferTimeout)
	}
}

func channelKey(session, id string) string {
	return session + "\x00" + id
}

var _ tunnel.Service = (*Service)(nil)
