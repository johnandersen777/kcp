// Copyright 2026 The KCP Authors.
// SPDX-License-Identifier: Apache-2.0

package browserstate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func validEnvelope() Envelope {
	return Envelope{
		Version:   CurrentVersion,
		KCP:       json.RawMessage(`{"objects":{}}`),
		Socialweb: json.RawMessage(`{"records":[]}`),
		FS:        json.RawMessage(`{"/":{}}`),
	}
}

func TestEnvelopeValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Envelope)
		want   string
	}{
		{name: "valid", mutate: func(*Envelope) {}},
		{
			name:   "unknown version",
			mutate: func(e *Envelope) { e.Version = 99 },
			want:   "unsupported version 99",
		},
		{
			name:   "missing version",
			mutate: func(e *Envelope) { e.Version = 0 },
			want:   "unsupported version 0",
		},
		{
			name:   "empty kcp",
			mutate: func(e *Envelope) { e.KCP = nil },
			want:   "kcp snapshot is empty",
		},
		{
			name:   "empty socialweb",
			mutate: func(e *Envelope) { e.Socialweb = json.RawMessage("") },
			want:   "socialweb snapshot is empty",
		},
		{
			name:   "empty fs",
			mutate: func(e *Envelope) { e.FS = json.RawMessage(" \n\t") },
			want:   "fs snapshot is empty",
		},
		{
			name:   "null kcp",
			mutate: func(e *Envelope) { e.KCP = json.RawMessage("null") },
			want:   "kcp snapshot is empty",
		},
		{
			name:   "invalid socialweb",
			mutate: func(e *Envelope) { e.Socialweb = json.RawMessage(`{"`) },
			want:   "socialweb snapshot is not valid JSON",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			envelope := validEnvelope()
			test.mutate(&envelope)
			err := envelope.Validate()
			if test.want == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", test.want)
			}
			if !errors.Is(err, ErrInvalidEnvelope) {
				t.Fatalf("Validate() error = %v, want it to wrap ErrInvalidEnvelope", err)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %q, want it to contain %q", err, test.want)
			}
		})
	}
}

func TestParseEnvelope(t *testing.T) {
	valid, err := json.Marshal(validEnvelope())
	if err != nil {
		t.Fatalf("marshal valid envelope: %v", err)
	}

	tests := []struct {
		name    string
		data    []byte
		wantErr string
	}{
		{name: "valid", data: valid},
		{
			name:    "unknown version",
			data:    []byte(`{"version":2,"kcp":{},"socialweb":{},"fs":{}}`),
			wantErr: "unsupported version 2",
		},
		{
			name:    "missing snapshots",
			data:    []byte(`{"version":1}`),
			wantErr: "kcp snapshot is empty",
		},
		{
			name:    "malformed json",
			data:    []byte(`{"version":1`),
			wantErr: "invalid envelope",
		},
		{
			name:    "empty input",
			data:    nil,
			wantErr: "invalid envelope",
		},
		{
			name:    "trailing data",
			data:    append(append([]byte{}, valid...), []byte(`{}`)...),
			wantErr: "invalid envelope",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			envelope, err := ParseEnvelope(test.data)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("ParseEnvelope() error = %v, want nil", err)
				}
				if envelope.Version != CurrentVersion {
					t.Fatalf("ParseEnvelope() version = %d, want %d", envelope.Version, CurrentVersion)
				}
				return
			}
			if err == nil {
				t.Fatalf("ParseEnvelope() = nil error, want %q", test.wantErr)
			}
			if !errors.Is(err, ErrInvalidEnvelope) {
				t.Fatalf("ParseEnvelope() error = %v, want it to wrap ErrInvalidEnvelope", err)
			}
			if !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("ParseEnvelope() error = %q, want it to contain %q", err, test.wantErr)
			}
		})
	}
}

func TestParseEnvelopeIgnoresUnknownFields(t *testing.T) {
	data := []byte(`{"version":1,"kcp":{},"socialweb":{},"fs":{},"future":{"x":1}}`)
	envelope, err := ParseEnvelope(data)
	if err != nil {
		t.Fatalf("ParseEnvelope() error = %v, want nil", err)
	}
	if envelope.Version != CurrentVersion {
		t.Fatalf("version = %d, want %d", envelope.Version, CurrentVersion)
	}
}

func TestMemoryBackend(t *testing.T) {
	ctx := context.Background()

	t.Run("save load delete", func(t *testing.T) {
		backend := NewMemoryBackend()
		if err := backend.Save(ctx, "kcp", []byte(`{"a":1}`)); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
		got, err := backend.Load(ctx, "kcp")
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if string(got) != `{"a":1}` {
			t.Fatalf("Load() = %q, want %q", got, `{"a":1}`)
		}
		if err := backend.Delete(ctx, "kcp"); err != nil {
			t.Fatalf("Delete() error = %v", err)
		}
		if _, err := backend.Load(ctx, "kcp"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Load() after Delete() error = %v, want ErrNotFound", err)
		}
	})

	t.Run("load missing key", func(t *testing.T) {
		backend := NewMemoryBackend()
		if _, err := backend.Load(ctx, "missing"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Load() error = %v, want ErrNotFound", err)
		}
	})

	t.Run("delete missing key is idempotent", func(t *testing.T) {
		backend := NewMemoryBackend()
		if err := backend.Delete(ctx, "missing"); err != nil {
			t.Fatalf("Delete() error = %v, want nil", err)
		}
	})

	t.Run("empty keys are rejected", func(t *testing.T) {
		backend := NewMemoryBackend()
		if _, err := backend.Load(ctx, ""); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("Load() error = %v, want ErrInvalidKey", err)
		}
		if err := backend.Save(ctx, "", []byte("x")); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("Save() error = %v, want ErrInvalidKey", err)
		}
		if err := backend.Delete(ctx, ""); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("Delete() error = %v, want ErrInvalidKey", err)
		}
	})

	t.Run("values are copied", func(t *testing.T) {
		backend := NewMemoryBackend()
		value := []byte("hello")
		if err := backend.Save(ctx, "k", value); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
		value[0] = 'j'

		got, err := backend.Load(ctx, "k")
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if string(got) != "hello" {
			t.Fatalf("Load() = %q, want %q", got, "hello")
		}

		got[0] = 'x'
		again, err := backend.Load(ctx, "k")
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		if string(again) != "hello" {
			t.Fatalf("Load() after mutating result = %q, want %q", again, "hello")
		}
	})

	t.Run("canceled context", func(t *testing.T) {
		backend := NewMemoryBackend()
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := backend.Load(canceled, "k"); !errors.Is(err, context.Canceled) {
			t.Fatalf("Load() error = %v, want context.Canceled", err)
		}
		if err := backend.Save(canceled, "k", nil); !errors.Is(err, context.Canceled) {
			t.Fatalf("Save() error = %v, want context.Canceled", err)
		}
		if err := backend.Delete(canceled, "k"); !errors.Is(err, context.Canceled) {
			t.Fatalf("Delete() error = %v, want context.Canceled", err)
		}
	})

	t.Run("concurrent access", func(t *testing.T) {
		backend := NewMemoryBackend()
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				key := fmt.Sprintf("k%d", i)
				for j := 0; j < 100; j++ {
					if err := backend.Save(ctx, key, []byte("v")); err != nil {
						t.Errorf("Save() error = %v", err)
						return
					}
					if _, err := backend.Load(ctx, key); err != nil {
						t.Errorf("Load() error = %v", err)
						return
					}
				}
			}(i)
		}
		wg.Wait()
	})
}
