package vault

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/component"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/marketidentity/atproto"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/vault/openbao"
)

func TestKVRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := NewStore()
	in := map[string]any{"token": "abc", "blob": []byte("raw"), "count": 1.5}
	if err := s.Write(ctx, "kv", "app/session", in); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out, err := s.Read(ctx, "kv", "app/session", 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("Read = %#v, want %#v", out, in)
	}
	out["token"] = "changed"
	out["blob"].([]byte)[0] = 'X'
	again, err := s.Read(ctx, "kv", "app/session", 0)
	if err != nil {
		t.Fatalf("Read again: %v", err)
	}
	if again["token"] != "abc" || string(again["blob"].([]byte)) != "raw" {
		t.Fatalf("stored data was aliased: %#v", again)
	}
	if _, err := s.Read(ctx, "kv", "kv/data/app/session", 0); err != nil {
		t.Fatalf("Read with data prefix: %v", err)
	}
}

func TestKVVersions(t *testing.T) {
	ctx := context.Background()
	s := NewStore()
	if err := s.Write(ctx, "kv", "k", map[string]any{"v": 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(ctx, "kv", "k", map[string]any{"v": 2}); err != nil {
		t.Fatal(err)
	}
	if data, err := s.Read(ctx, "kv", "k", 2); err != nil || data["v"] != 2 {
		t.Fatalf("Read version 2 = %#v, %v", data, err)
	}
	if _, err := s.Read(ctx, "kv", "k", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Read version 1 error = %v, want ErrNotFound", err)
	}
	if _, err := s.Read(ctx, "kv", "k", 3); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Read version 3 error = %v, want ErrNotFound", err)
	}
	out, err := s.Read(ctx, "kv", "k", 0)
	if err != nil || out["v"] != 2 {
		t.Fatalf("Read latest = %#v, %v", out, err)
	}
}

func TestKVNestedPathsAndList(t *testing.T) {
	ctx := context.Background()
	s := NewStore()
	for _, tc := range []struct{ mount, path, value string }{
		{"kv", "accounts/alice/session", "alice"},
		{"kv", "accounts/bob/session", "bob"},
		{"kv", "teams/a/b/c", "deep"},
		{"kv/apps", "alice/session", "nested-mount"},
		// Same composite "a/b/c" when joined, but distinct mount/path pairs.
		{"a/b", "c", "nested-mount-key"},
		{"a", "b/c", "nested-path-key"},
	} {
		if err := s.Write(ctx, tc.mount, tc.path, map[string]any{"v": tc.value}); err != nil {
			t.Fatalf("Write %s/%s: %v", tc.mount, tc.path, err)
		}
	}
	cases := []struct {
		mount, path string
		want        []string
	}{
		{"kv", "accounts", []string{"alice/", "bob/"}},
		{"kv", "accounts/alice", []string{"session"}},
		{"kv", "teams/a", []string{"b/"}},
		{"kv", "teams/a/b", []string{"c"}},
		{"kv/apps", "", []string{"alice/"}},
		{"kv", "missing", []string{}},
	}
	for _, tc := range cases {
		got, err := s.List(ctx, tc.mount, tc.path)
		if err != nil {
			t.Fatalf("List %s/%s: %v", tc.mount, tc.path, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("List %s/%s = %v, want %v", tc.mount, tc.path, got, tc.want)
		}
	}
	for _, tc := range []struct{ mount, path, want string }{
		{"a/b", "c", "nested-mount-key"},
		{"a", "b/c", "nested-path-key"},
	} {
		data, err := s.Read(ctx, tc.mount, tc.path, 0)
		if err != nil || data["v"] != tc.want {
			t.Fatalf("Read %s/%s = %#v, %v; want %q", tc.mount, tc.path, data, err, tc.want)
		}
	}
}

func TestKVDelete(t *testing.T) {
	ctx := context.Background()
	s := NewStore()
	if err := s.Write(ctx, "kv", "gone", map[string]any{"v": 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "kv", "gone"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Read(ctx, "kv", "gone", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Read after Delete error = %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, "kv", "gone"); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
}

func TestKVSnapshotRestore(t *testing.T) {
	ctx := context.Background()
	s := NewStore()
	if err := s.Write(ctx, "kv", "a", map[string]any{"v": "a"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(ctx, "kv", "nested/b", map[string]any{"v": "b"}); err != nil {
		t.Fatal(err)
	}
	snap := s.Snapshot()
	if len(snap.Entries) != 2 {
		t.Fatalf("Snapshot entries = %d, want 2", len(snap.Entries))
	}
	if err := s.Delete(ctx, "kv", "a"); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(ctx, "kv", "c", map[string]any{"v": "c"}); err != nil {
		t.Fatal(err)
	}
	s.Restore(snap)
	if _, err := s.Read(ctx, "kv", "a", 0); err != nil {
		t.Fatalf("restored a: %v", err)
	}
	if _, err := s.Read(ctx, "kv", "c", 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("c survived Restore: %v", err)
	}
	snap.Entries[0].Data["v"] = "mutated"
	restored, err := s.Read(ctx, "kv", snap.Entries[0].Path, 0)
	if err != nil {
		t.Fatalf("read after snapshot mutation: %v", err)
	}
	if restored["v"] == "mutated" {
		t.Fatal("Restore aliased snapshot data")
	}
}

func TestKVSnapshotJSON(t *testing.T) {
	ctx := context.Background()
	s := NewStore()
	if err := s.Write(ctx, "kv", "nested/key", map[string]any{"v": "value", "n": 2.0}); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(s.Snapshot())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded Snapshot
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	var restored Store
	restored.Restore(decoded)
	data, err := restored.Read(ctx, "kv", "nested/key", 0)
	if err != nil || data["v"] != "value" {
		t.Fatalf("Read after JSON restore = %#v, %v", data, err)
	}
}

func TestKVConcurrent(t *testing.T) {
	ctx := context.Background()
	s := NewStore()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = s.Write(ctx, "kv", "shared", map[string]any{"v": j})
				_, _ = s.Read(ctx, "kv", "shared", 0)
				_, _ = s.List(ctx, "kv", "")
				_ = s.Snapshot()
			}
		}()
	}
	wg.Wait()
	if _, err := s.Read(ctx, "kv", "shared", 0); err != nil {
		t.Fatalf("Read after concurrent writes: %v", err)
	}
}

func TestKVContextCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := NewStore()
	if _, err := s.Read(ctx, "kv", "k", 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("Read error = %v, want context.Canceled", err)
	}
	if err := s.Write(ctx, "kv", "k", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Write error = %v, want context.Canceled", err)
	}
	if err := s.Delete(ctx, "kv", "k"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Delete error = %v, want context.Canceled", err)
	}
	if _, err := s.List(ctx, "kv", ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("List error = %v, want context.Canceled", err)
	}
}

func TestKVInvalidRef(t *testing.T) {
	s := NewStore()
	if _, err := s.Read(context.Background(), "", "k", 0); !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("Read error = %v, want ErrInvalidRef", err)
	}
	if err := s.Write(context.Background(), "kv", "/", nil); !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("Write error = %v, want ErrInvalidRef", err)
	}
}

func TestSecretsRoundTrip(t *testing.T) {
	ctx := context.Background()
	store := NewStore()
	secrets := NewSecrets(store)
	ref := commonv1.SecretRef{Mount: "kv", Path: "identities/alice/keys", Key: "ignored"}
	in := map[string]any{"password": "hunter2", "privateKey": "secret"}
	if err := secrets.Put(ctx, ref, in); err != nil {
		t.Fatalf("Put: %v", err)
	}
	out, err := secrets.Get(ctx, ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("Get = %#v, want %#v", out, in)
	}
	// The secret is visible through the shared KV as well.
	data, err := store.Read(ctx, "kv", "identities/alice/keys", 0)
	if err != nil || data["password"] != "hunter2" {
		t.Fatalf("KV Read = %#v, %v", data, err)
	}
	missing := commonv1.SecretRef{Mount: "kv", Path: "identities/bob/keys"}
	if _, err := secrets.Get(ctx, missing); !errors.Is(err, atproto.ErrSecretNotFound) {
		t.Fatalf("Get missing error = %v, want atproto.ErrSecretNotFound", err)
	}
	empty := commonv1.SecretRef{Mount: "kv", Path: "empty"}
	if err := secrets.Put(ctx, empty, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if _, err := secrets.Get(ctx, empty); !errors.Is(err, atproto.ErrSecretNotFound) {
		t.Fatalf("Get empty error = %v, want atproto.ErrSecretNotFound", err)
	}
	if err := secrets.Delete(ctx, ref); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := secrets.Get(ctx, ref); !errors.Is(err, atproto.ErrSecretNotFound) {
		t.Fatalf("Get after Delete error = %v, want atproto.ErrSecretNotFound", err)
	}
	if err := secrets.Delete(ctx, ref); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
}

func TestSecretsSnapshotRestore(t *testing.T) {
	ctx := context.Background()
	secrets := NewSecrets(nil)
	ref := commonv1.SecretRef{Mount: "kv", Path: "session"}
	if err := secrets.Put(ctx, ref, map[string]any{"state": "pending"}); err != nil {
		t.Fatal(err)
	}
	snap := secrets.Snapshot()
	if err := secrets.Delete(ctx, ref); err != nil {
		t.Fatal(err)
	}
	secrets.Restore(snap)
	data, err := secrets.Get(ctx, ref)
	if err != nil || data["state"] != "pending" {
		t.Fatalf("Get after Restore = %#v, %v", data, err)
	}
}

func TestCredentialsResolve(t *testing.T) {
	ctx := context.Background()
	store := NewStore()
	creds := NewCredentials(store)
	ref := component.CredentialRef{Mount: "kv", Path: "pds", Keys: []string{"password"}}
	if err := creds.WriteCredential(ctx, ref, map[string]any{"password": "pw", "user": "alice"}); err != nil {
		t.Fatalf("WriteCredential: %v", err)
	}
	cred, err := creds.Resolve(ctx, ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !reflect.DeepEqual(cred.Data, map[string]any{"password": "pw"}) {
		t.Fatalf("filtered Resolve = %#v", cred.Data)
	}
	if !reflect.DeepEqual(cred.Ref, ref) {
		t.Fatalf("Resolve ref = %#v, want %#v", cred.Ref, ref)
	}
	all := ref
	all.Keys = nil
	cred, err = creds.Resolve(ctx, all)
	if err != nil || cred.Data["user"] != "alice" {
		t.Fatalf("unfiltered Resolve = %#v, %v", cred.Data, err)
	}
	missing := component.CredentialRef{Mount: "kv", Path: "missing"}
	if _, err := creds.Resolve(ctx, missing); !errors.Is(err, ErrNotFound) || !errors.Is(err, openbao.ErrNotFound) {
		t.Fatalf("Resolve missing error = %v, want ErrNotFound wrapping openbao.ErrNotFound", err)
	}
	if err := creds.WriteCredential(ctx, missing, nil); err == nil {
		t.Fatal("WriteCredential nil data succeeded")
	}
	if _, err := creds.Resolve(ctx, component.CredentialRef{Path: "x"}); err == nil {
		t.Fatal("Resolve without mount succeeded")
	}
	if err := creds.DeleteCredential(ctx, all); err != nil {
		t.Fatalf("DeleteCredential: %v", err)
	}
	if _, err := creds.Resolve(ctx, all); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve after Delete error = %v, want ErrNotFound", err)
	}
}

func TestCredentialsVersion(t *testing.T) {
	ctx := context.Background()
	creds := NewCredentials(NewStore())
	ref := component.CredentialRef{Mount: "kv", Path: "pds"}
	if err := creds.WriteCredential(ctx, ref, map[string]any{"password": "one"}); err != nil {
		t.Fatal(err)
	}
	if err := creds.WriteCredential(ctx, ref, map[string]any{"password": "two"}); err != nil {
		t.Fatal(err)
	}
	old := ref
	old.Version = 1
	if _, err := creds.Resolve(ctx, old); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve version 1 error = %v, want ErrNotFound", err)
	}
	current := ref
	current.Version = 2
	cred, err := creds.Resolve(ctx, current)
	if err != nil || cred.Data["password"] != "two" {
		t.Fatalf("Resolve version 2 = %#v, %v", cred.Data, err)
	}
}
