// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

// Package browserstate defines the snapshot contract shared with the browser
// state store (scripts/browser/state_store.js) and the storage seam the js/wasm
// build will wire to host functions.
package browserstate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// CurrentVersion is the only envelope version this build understands.
const CurrentVersion = 1

var (
	ErrInvalidEnvelope = errors.New("browserstate: invalid envelope")
	ErrNotFound        = errors.New("browserstate: key not found")
	ErrInvalidKey      = errors.New("browserstate: key must be a non-empty string")
)

// Envelope is the persisted browser snapshot. Each snapshot is the JSON
// document the corresponding subsystem produced; the store never interprets
// it.
type Envelope struct {
	Version   int             `json:"version"`
	KCP       json.RawMessage `json:"kcp"`
	Socialweb json.RawMessage `json:"socialweb"`
	FS        json.RawMessage `json:"fs"`
}

// Validate rejects unknown versions, empty snapshots and invalid JSON.
func (e Envelope) Validate() error {
	if e.Version != CurrentVersion {
		return fmt.Errorf("%w: unsupported version %d (want %d)", ErrInvalidEnvelope, e.Version, CurrentVersion)
	}
	snapshots := []struct {
		name string
		raw  json.RawMessage
	}{
		{"kcp", e.KCP},
		{"socialweb", e.Socialweb},
		{"fs", e.FS},
	}
	for _, snapshot := range snapshots {
		trimmed := bytes.TrimSpace(snapshot.raw)
		if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
			return fmt.Errorf("%w: %s snapshot is empty", ErrInvalidEnvelope, snapshot.name)
		}
		if !json.Valid(trimmed) {
			return fmt.Errorf("%w: %s snapshot is not valid JSON", ErrInvalidEnvelope, snapshot.name)
		}
	}
	return nil
}

// ParseEnvelope decodes and validates a persisted snapshot envelope.
func ParseEnvelope(data []byte) (Envelope, error) {
	var envelope Envelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return Envelope{}, fmt.Errorf("%w: %v", ErrInvalidEnvelope, err)
	}
	if err := envelope.Validate(); err != nil {
		return Envelope{}, err
	}
	return envelope, nil
}

// Backend is the storage seam. The js/wasm build will back it with host
// functions over scripts/browser/state_store.js; tests use MemoryBackend.
type Backend interface {
	Load(ctx context.Context, key string) ([]byte, error)
	Save(ctx context.Context, key string, value []byte) error
	Delete(ctx context.Context, key string) error
}

// MemoryBackend keeps snapshots in process memory. It is safe for concurrent
// use and copies values on the way in and out.
type MemoryBackend struct {
	mu     sync.RWMutex
	values map[string][]byte
}

func NewMemoryBackend() *MemoryBackend {
	return &MemoryBackend{values: map[string][]byte{}}
}

func (m *MemoryBackend) Load(ctx context.Context, key string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if key == "" {
		return nil, ErrInvalidKey
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	value, ok := m.values[key]
	if !ok {
		return nil, ErrNotFound
	}
	return bytes.Clone(value), nil
}

func (m *MemoryBackend) Save(ctx context.Context, key string, value []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if key == "" {
		return ErrInvalidKey
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.values[key] = bytes.Clone(value)
	return nil
}

func (m *MemoryBackend) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if key == "" {
		return ErrInvalidKey
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.values, key)
	return nil
}
