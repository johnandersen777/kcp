package eventstream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	contractmarket "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/market"
)

const (
	// InboxAdapter is the StateSync adapter the requester consumes.
	InboxAdapter = "requester.inbox"

	inboxLabel = "socialweb.computer/inbox"

	inboxPrefix = "inbox-"

	pollLimit = 100

	// pollPages bounds a single poll; the real requester's listAll uses the
	// same ceiling.
	pollPages = 20
)

// guestEventNSIDs are the payload collections the requester applies
// (internal/market/requester/events.go).
var guestEventNSIDs = [...]string{
	market.ComputeEventsVMOnNetworkNSID,
	market.ComputeEventsVMHostKeysNSID,
	market.ComputeEventsVMTerminatedNSID,
}

// RecordLister is the market.RepoStore.ListRecords subset the poller reads.
// *market.RepoStore and market.RecordLister satisfy it.
type RecordLister interface {
	ListRecords(ctx context.Context, repo, collection string, limit int, cursor string) (market.ListPage, error)
}

// StateSyncCreator is the contractmarket.StateSyncIntents subset the poller
// writes. It lets the orchestrator back the inbox with its own StateSync store.
type StateSyncCreator interface {
	CreateStateSync(ctx context.Context, sync v1alpha1.StateSync) error
}

// Poller lists a bidder's market event records and turns the ones addressed to
// a receipt into requester.inbox StateSync mailboxes. It resolves no records
// and verifies no signatures: the requester does that when it consumes the
// mailbox.
type Poller struct {
	lister RecordLister

	creator StateSyncCreator

	mu sync.Mutex

	created map[string]bool
}

// PollerOptions inject the read and write surfaces.
type PollerOptions struct {
	Lister RecordLister

	Creator StateSyncCreator
}

// NewPoller returns a poller with an empty in-memory dedupe set.
func NewPoller(opts PollerOptions) (*Poller, error) {
	if opts.Lister == nil {
		return nil, errors.New("eventstream: the poller needs a record lister")
	}
	if opts.Creator == nil {
		return nil, errors.New("eventstream: the poller needs a StateSync creator")
	}
	return &Poller{lister: opts.Lister, creator: opts.Creator, created: map[string]bool{}}, nil
}

// Poll lists bidderDID's event records, keeps those whose receipt is receiptURI
// and whose payload is onNetwork, hostKeys, or terminated, and creates one
// requester.inbox StateSync per unseen event. It returns how many mailboxes it
// created in this call.
func (p *Poller) Poll(ctx context.Context, bidderDID, receiptURI string) (int, error) {
	did := strings.TrimSpace(bidderDID)
	receipt := strings.TrimSpace(receiptURI)
	if did == "" || receipt == "" {
		return 0, errors.New("eventstream: the poller needs a bidder DID and a receipt URI")
	}
	records, err := p.listAll(ctx, did)
	if err != nil {
		return 0, err
	}
	created := 0
	var errs []error
	for _, record := range records {
		event, ok := inboxReceiptEvent(record, receipt)
		if !ok {
			continue
		}
		name := InboxName(receipt, event.URI, event.CID)
		if p.seen(name) {
			continue
		}
		err := p.creator.CreateStateSync(ctx, inboxStateSync(name, event))
		switch {
		case err == nil:
			p.mark(name)
			created++
		case errors.Is(err, contractmarket.ErrExists):
			p.mark(name)
		default:
			errs = append(errs, fmt.Errorf("eventstream: creating %s: %w", name, err))
		}
	}
	return created, errors.Join(errs...)
}

func (p *Poller) listAll(ctx context.Context, did string) ([]market.ListedRecord, error) {
	out := []market.ListedRecord{}
	seen := map[string]bool{}
	cursor := ""
	for range pollPages {
		page, err := p.lister.ListRecords(ctx, did, market.EventNSID, pollLimit, cursor)
		if err != nil {
			return out, fmt.Errorf("eventstream: listing %s in %s: %w", market.EventNSID, did, err)
		}
		out = append(out, page.Records...)
		if page.Cursor == "" || seen[page.Cursor] {
			return out, nil
		}
		seen[page.Cursor] = true
		cursor = page.Cursor
	}
	return out, nil
}

// inboxReceiptEvent reads the event envelope without resolving either strong
// reference, mirroring internal/market/requester's receiptEvent.
func inboxReceiptEvent(record market.ListedRecord, receiptURI string) (v1alpha1.ComputeContractEvent, bool) {
	if strings.TrimSpace(record.URI) == "" || strings.TrimSpace(record.CID) == "" {
		return v1alpha1.ComputeContractEvent{}, false
	}
	receipt, _ := record.Value["receipt"].(map[string]any)
	if stringOf(receipt["uri"]) != receiptURI {
		return v1alpha1.ComputeContractEvent{}, false
	}
	payload, _ := record.Value["payload"].(map[string]any)
	uri := stringOf(payload["uri"])
	if uri == "" || !isGuestEventNSID(market.NSIDFromURI(uri)) {
		return v1alpha1.ComputeContractEvent{}, false
	}
	return v1alpha1.ComputeContractEvent{URI: record.URI, CID: record.CID, ReceiptURI: receiptURI, ReceivedAt: market.NowISO()}, true
}

func stringOf(value any) string {
	s, _ := value.(string)
	return s
}

func isGuestEventNSID(nsid string) bool {
	return slices.Contains(guestEventNSIDs[:], nsid)
}

// InboxName is the same sha256 name internal/market/requester uses, so a
// mailbox created here is recognised there.
func InboxName(receiptURI, eventURI, cid string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(receiptURI) + "\x00" + eventURI + "#" + cid))
	return inboxPrefix + hex.EncodeToString(sum[:16])
}

type inboxEventPayload struct {
	Event v1alpha1.ComputeContractEvent `json:"event"`
}

// inboxStateSync is the exact shape internal/market/requester's mailbox()
// writes for a requester.inbox event.
func inboxStateSync(name string, event v1alpha1.ComputeContractEvent) v1alpha1.StateSync {
	payload, _ := json.Marshal(inboxEventPayload{Event: event})
	return v1alpha1.StateSync{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: contractmarket.KindStateSync},
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{inboxLabel: "true"}},
		Spec: v1alpha1.StateSyncSpec{
			Sink:           v1alpha1.StateSyncSink{Adapter: InboxAdapter},
			Direction:      v1alpha1.StateSyncDirectionOutbound,
			Payload:        payload,
			IdempotencyKey: "inbox:" + event.URI + "#" + event.CID,
			ConflictPolicy: v1alpha1.StateSyncConflictSourceWins,
			DeletionPolicy: v1alpha1.StateSyncDeletionRetain,
		},
	}
}

// ponytail: the dedupe set lives for the Poller's lifetime and grows with the
// bidder's event history. One poller per requester (or a periodic Reset) is
// enough until memory shows up in a profile.
func (p *Poller) seen(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.created[name]
}

func (p *Poller) mark(name string) {
	p.mu.Lock()
	p.created[name] = true
	p.mu.Unlock()
}
