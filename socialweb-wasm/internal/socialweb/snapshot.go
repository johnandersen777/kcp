package socialweb

import (
	"encoding/json"
	"fmt"

	"github.com/johnandersen777/socialweb-computer-kcp/wasm/internal/browserinfra/vaulthost"
)

// SnapshotVersion is the only socialweb browser snapshot this build reads.
const SnapshotVersion = 1

// Snapshot is the browser state that must survive a worker reload. OAuth
// sessions and pending PKCE states both live in the vault (see the
// oauthclient.VaultStateStore wiring in Start), so one vault snapshot covers
// them.
type Snapshot struct {
	Version int                `json:"version"`
	Vault   vaulthost.Snapshot `json:"vault"`
}

// ParseSnapshot validates a JSON snapshot restored from IndexedDB. An empty
// value is the caller's business; this rejects everything else invalid.
func ParseSnapshot(raw []byte) (Snapshot, error) {
	var snapshot Snapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return Snapshot{}, fmt.Errorf("socialweb: decoding the browser snapshot: %w", err)
	}
	if snapshot.Version != SnapshotVersion {
		return Snapshot{}, fmt.Errorf("socialweb: unsupported browser snapshot version %d (want %d)",
			snapshot.Version, SnapshotVersion)
	}
	return snapshot, nil
}

// planRestore decides what Start does with the restored value raw: nothing
// to restore (empty), a snapshot to restore, or, when raw is not empty but
// cannot be read, an error and persist false. Without persist the vault never
// hands a snapshot to OnSnapshot, so the first vault write cannot save an
// empty vault over the stored one; the page keeps the unreadable value.
func planRestore(raw []byte) (snapshot *Snapshot, persist bool, err error) {
	if len(raw) == 0 {
		return nil, true, nil
	}
	parsed, err := ParseSnapshot(raw)
	if err != nil {
		return nil, false, err
	}
	return &parsed, true, nil
}
