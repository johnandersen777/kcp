package socialweb

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	bvault "github.com/johnandersen777/socialweb-computer-kcp/wasm/internal/browserinfra/vault"
)

// DefaultSecretsMount is the requester's KV mount
// (marketrequester.RequesterVaultClient), the one `run request` writes.
const DefaultSecretsMount = "requester"

var contractSecretsPath = regexp.MustCompile(`^contracts/[a-z0-9]([-a-z0-9]*[a-z0-9])?/secrets$`)

// ContractSecrets is a ComputeContract secrets bundle the page writes before
// it creates the contract. Bundle is the JSON text of
// [{"path":"/abs/guest/path","value":"..."}].
type ContractSecrets struct {
	Workspace string `json:"workspace,omitempty"`
	Mount     string `json:"mount,omitempty"`
	Path      string `json:"path"`
	Bundle    string `json:"bundle"`
}

// PutContractSecrets writes the bundle where the requester reads
// spec.secretsRef: the workspace's browser vault store, KV data
// {"bundle": <json text>} at mount/path, as stack/request.PutSecrets does.
// Only contracts/<name>/secrets paths are writable, so the page cannot reach
// OAuth sessions or ingress keys through this entry point.
func PutContractSecrets(ctx context.Context, store *bvault.Store, in ContractSecrets) error {
	if store == nil {
		return errors.New("socialweb: no vault store")
	}
	mount := cmp.Or(strings.TrimSpace(in.Mount), DefaultSecretsMount)
	if mount != DefaultSecretsMount {
		return fmt.Errorf("socialweb: contract secrets live on the %q mount, not %q", DefaultSecretsMount, mount)
	}
	if !contractSecretsPath.MatchString(in.Path) {
		return fmt.Errorf("socialweb: %q is not a contracts/<name>/secrets path", in.Path)
	}
	var entries []struct {
		Path  string `json:"path"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal([]byte(in.Bundle), &entries); err != nil {
		return fmt.Errorf("socialweb: the secrets bundle: %w", err)
	}
	if len(entries) == 0 {
		return errors.New("socialweb: the secrets bundle is empty")
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Path, "/") {
			return fmt.Errorf("socialweb: the secrets bundle names the path %q", e.Path)
		}
	}
	return store.Write(ctx, mount, in.Path, map[string]any{"bundle": in.Bundle})
}

// PutContractSecrets writes a bundle into the workspace's browser vault.
func (o *Orchestrator) PutContractSecrets(ctx context.Context, in ContractSecrets) error {
	if o == nil || o.Vault == nil {
		return errors.New("socialweb: the vault is not running")
	}
	return PutContractSecrets(ctx, o.Vault.Provider().Store(cmp.Or(strings.TrimSpace(in.Workspace), "root")), in)
}
