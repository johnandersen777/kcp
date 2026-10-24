package socialweb

import (
	"context"
	"encoding/json"
	"testing"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	contractvault "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/vault"

	"github.com/johnandersen777/socialweb-computer-kcp/wasm/internal/browserinfra/vaulthost"
)

// The bundle the page writes reads back through the requester's path:
// services/requester secretsVault resolves vault.read on its Vault with scope
// mount/path, then market/requester readSecretsBundle reads data["bundle"].
func TestPutContractSecretsReadsBackLikeTheRequester(t *testing.T) {
	ctx := context.Background()
	provider := vaulthost.NewProvider()
	b := bus.New(ctx)
	t.Cleanup(func() { _ = b.Stop() })
	provider.Register(b)
	vault := commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: "Vault", Name: "requester", Workspace: "root"}
	provider.Bind(vaulthost.Binding{Vault: vault, Mount: vaulthost.DefaultKVMount})

	bundle := `[{"path":"/etc/wootty/wootty.env","value":"WOOTTY_AUTH_TOKEN=abc\n"}]`
	in := ContractSecrets{Path: "contracts/vmshell-ab12cd/secrets", Bundle: bundle}
	if err := PutContractSecrets(ctx, provider.Store("root"), in); err != nil {
		t.Fatal(err)
	}

	ref := commonv1.SecretRef{Mount: DefaultSecretsMount, Path: in.Path}
	kv, err := bus.Resolve[contractvault.KV](bus.WithCaller(ctx, vault), b, vault, vault,
		bus.Capability{Name: contractvault.CapabilityRead, Scope: []string{ref.Mount + "/" + ref.Path}})
	if err != nil {
		t.Fatal(err)
	}
	data, err := kv.Read(ctx, ref.Mount, ref.Path, 0)
	if err != nil {
		t.Fatal(err)
	}
	text, _ := data["bundle"].(string)
	var entries []struct{ Path, Value string }
	if err := json.Unmarshal([]byte(text), &entries); err != nil || len(entries) != 1 ||
		entries[0].Path != "/etc/wootty/wootty.env" || entries[0].Value != "WOOTTY_AUTH_TOKEN=abc\n" {
		t.Fatalf("bundle = %q, %v", text, err)
	}
}

func TestPutContractSecretsRejects(t *testing.T) {
	ctx := context.Background()
	store := vaulthost.NewProvider().Store("root")
	ok := `[{"path":"/a","value":"b"}]`
	for _, in := range []ContractSecrets{
		{Path: "oauth/requester", Bundle: ok},
		{Path: "contracts/x/secrets/../../oauth", Bundle: ok},
		{Path: "contracts/Bad/secrets", Bundle: ok},
		{Mount: "secret", Path: "contracts/x/secrets", Bundle: ok},
		{Path: "contracts/x/secrets", Bundle: `[{"path":"relative","value":"b"}]`},
		{Path: "contracts/x/secrets", Bundle: `[]`},
		{Path: "contracts/x/secrets", Bundle: `not json`},
	} {
		if err := PutContractSecrets(ctx, store, in); err == nil {
			t.Fatalf("accepted %#v", in)
		}
	}
}
