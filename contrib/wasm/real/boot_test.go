package real

import (
	"context"
	"testing"
	"time"
)

func TestBootNative(t *testing.T) {
	if testing.Short() {
		t.Skip("boots the full apiserver")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	s, err := Boot(ctx, Config{
		RootDir:      t.TempDir(),
		Address:      "127.0.0.1:16443",
		ShardBaseURL: "http://kcp.local",
		ReadyTimeout: 2 * time.Minute,
	})
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	defer s.Close()
	resp, err := s.Client.Get(s.BaseURL + "/readyz")
	if err != nil {
		t.Fatalf("readyz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("readyz status %d", resp.StatusCode)
	}
}
