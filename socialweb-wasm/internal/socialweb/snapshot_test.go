package socialweb

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/atcrypto"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/oauthclient"
	mkatproto "github.com/johnandersen777/socialweb-computer-kcp/internal/marketidentity/atproto"
	"github.com/johnandersen777/socialweb-computer-kcp/wasm/internal/browserinfra/vault"
	"github.com/johnandersen777/socialweb-computer-kcp/wasm/internal/browserinfra/vaulthost"
)

// TestSnapshotRoundTripRestoresSessionAndOAuthState is the reload contract: a
// vault snapshot taken before a reload restores the MarketIdentity OAuth
// session and a pending PKCE authorization state on the other side.
func TestSnapshotRoundTripRestoresSessionAndOAuthState(t *testing.T) {
	ctx := context.Background()
	provider := vaulthost.NewProvider()
	secrets := vault.NewSecrets(provider.Store("root"))

	key, err := atcrypto.GeneratePrivateKeyP256()
	if err != nil {
		t.Fatalf("generate DPoP key: %v", err)
	}
	sessionData := oauthclient.SessionData{
		AccountDID:              "did:plc:alice",
		SessionID:               "session-1",
		HostURL:                 "https://pds.example",
		ClientID:                "https://app.example/oauth-client-metadata.json",
		AuthServerURL:           "https://auth.example",
		AuthServerTokenEndpoint: "https://auth.example/token",
		Scopes:                  []string{"atproto"},
		AccessToken:             testAccessToken(time.Now().Add(time.Hour)),
		RefreshToken:            "refresh",
		DPoPPrivateKeyMultibase: key.Multibase(),
	}
	sessionRef := commonv1.SecretRef{Mount: "requester", Path: "identity/requester/pds-session"}
	rawSession, err := json.Marshal(sessionData)
	if err != nil {
		t.Fatal(err)
	}
	if err := secrets.Put(ctx, sessionRef, map[string]any{"session": string(rawSession)}); err != nil {
		t.Fatalf("Put session: %v", err)
	}
	stateRef := commonv1.SecretRef{Mount: "requester", Path: "oauth/requester"}
	states := &mkatproto.VaultStateStore{Vault: secrets, Ref: stateRef}
	pending := oauthclient.PendingAuth{
		State:                   "state-1",
		CodeVerifier:            "pkce-verifier",
		RedirectURI:             "https://app.example/oauth/callback/requester",
		ClientID:                sessionData.ClientID,
		HostURL:                 sessionData.HostURL,
		TokenEndpoint:           sessionData.AuthServerTokenEndpoint,
		DPoPPrivateKeyMultibase: key.Multibase(),
	}
	if err := states.Put(ctx, pending); err != nil {
		t.Fatalf("Put pending state: %v", err)
	}

	raw, err := json.Marshal(Snapshot{Version: SnapshotVersion, Vault: provider.Snapshot()})
	if err != nil {
		t.Fatalf("Marshal snapshot: %v", err)
	}
	parsed, err := ParseSnapshot(raw)
	if err != nil {
		t.Fatalf("ParseSnapshot: %v", err)
	}
	restored := vaulthost.NewProvider()
	restored.Restore(parsed.Vault)

	restoredSecrets := vault.NewSecrets(restored.Store("root"))
	data, err := restoredSecrets.Get(ctx, sessionRef)
	if err != nil {
		t.Fatalf("Get restored session: %v", err)
	}
	if data["session"] != string(rawSession) {
		t.Fatalf("restored session = %#v, want %q", data["session"], rawSession)
	}

	// A live access token means the identity controller's restoreOAuthSession
	// opens the session instead of beginning a new browser authorization.
	var gotSession oauthclient.SessionData
	if err := json.Unmarshal([]byte(data["session"].(string)), &gotSession); err != nil {
		t.Fatalf("decode restored session: %v", err)
	}
	session, err := oauthclient.Open(ctx, oauthclient.Options{
		DID:     gotSession.AccountDID,
		RepoDID: gotSession.AccountDID,
		PDSURL:  gotSession.HostURL,
		// No Directory, HTTP client, or network: a live token must not need them.
		ClientID: gotSession.ClientID,
		Scopes:   gotSession.Scopes,
		Store:    staticSessionStore{data: gotSession},
	})
	if err != nil {
		t.Fatalf("Open restored session: %v", err)
	}
	if session.DID() != gotSession.AccountDID {
		t.Fatalf("restored session DID = %q, want %q", session.DID(), gotSession.AccountDID)
	}

	got, ok, err := (&mkatproto.VaultStateStore{Vault: restoredSecrets, Ref: stateRef}).Take(ctx, pending.State)
	if err != nil {
		t.Fatalf("Take restored state: %v", err)
	}
	if !ok || got.CodeVerifier != pending.CodeVerifier || got.DPoPPrivateKeyMultibase != pending.DPoPPrivateKeyMultibase {
		t.Fatalf("restored pending auth = %#v, %v; want PKCE verifier %q", got, ok, pending.CodeVerifier)
	}
}

func TestParseSnapshotVersions(t *testing.T) {
	valid := Snapshot{Version: SnapshotVersion}
	raw, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseSnapshot(raw); err != nil {
		t.Fatalf("ParseSnapshot(valid) = %v", err)
	}
	for name, input := range map[string]string{
		"malformed":       `{"version":`,
		"missing version": `{"vault":{"workspaces":{}}}`,
		"future version":  `{"version":2,"vault":{"workspaces":{}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseSnapshot([]byte(input)); err == nil {
				t.Fatalf("ParseSnapshot(%s) = nil error, want rejection", input)
			}
		})
	}
}

// staticSessionStore serves one session without touching disk or network.
type staticSessionStore struct {
	data oauthclient.SessionData
}

func (s staticSessionStore) List(context.Context) ([]oauthclient.SessionData, error) {
	return []oauthclient.SessionData{s.data}, nil
}

func (staticSessionStore) Save(context.Context, oauthclient.SessionData) error { return nil }

func (staticSessionStore) Delete(context.Context, string, string) error { return nil }

// testAccessToken is an unsigned JWT: oauthclient only reads the exp claim.
func testAccessToken(exp time.Time) string {
	encode := base64.RawURLEncoding.EncodeToString
	return encode([]byte(`{"alg":"ES256","typ":"JWT"}`)) + "." +
		encode([]byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix()))) + "."
}
