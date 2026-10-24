package eventstream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/atproto/market"
	contractmarket "github.com/johnandersen777/socialweb-computer-kcp/internal/contract/market"
)

const (
	testBidderDID = "did:plc:bidder"

	testReceiptURI = "at://did:plc:requester/com.publicdomainrelay.temp.market.receipt/r1"

	testCID = "bafyreidfayvfuwqa7qlnopdjiqrxzs6blmoeu4rujcjtnci5beludirz2a"
)

type fakeLister struct {
	pages []market.ListPage

	repo, collection string

	calls int

	err error
}

func (f *fakeLister) ListRecords(_ context.Context, repo, collection string, _ int, _ string) (market.ListPage, error) {
	f.calls++
	f.repo, f.collection = repo, collection
	if f.err != nil {
		return market.ListPage{}, f.err
	}
	if len(f.pages) == 0 {
		return market.ListPage{}, nil
	}
	page := f.pages[0]
	f.pages = f.pages[1:]
	return page, nil
}

type fakeCreator struct {
	syncs []v1alpha1.StateSync

	calls int

	err error
}

func (f *fakeCreator) CreateStateSync(_ context.Context, sync v1alpha1.StateSync) error {
	f.calls++
	if f.err != nil {
		return f.err
	}
	f.syncs = append(f.syncs, sync)
	return nil
}

func eventRecord(rkey, receiptURI, payloadNSID string) market.ListedRecord {
	return market.ListedRecord{
		URI: "at://" + testBidderDID + "/" + market.EventNSID + "/" + rkey,
		CID: testCID,
		Value: map[string]any{
			"$type":   market.EventNSID,
			"receipt": map[string]any{"$type": market.StrongRefNSID, "uri": receiptURI, "cid": testCID},
			"payload": map[string]any{"$type": market.StrongRefNSID, "uri": "at://" + testBidderDID + "/" + payloadNSID + "/p-" + rkey, "cid": testCID},
		},
	}
}

func (f *fakeLister) page(records ...market.ListedRecord) {
	f.pages = []market.ListPage{{Records: records}}
}

func TestPollerCreatesInboxMailboxes(t *testing.T) {
	lister := &fakeLister{}
	lister.page(
		eventRecord("on-network", testReceiptURI, market.ComputeEventsVMOnNetworkNSID),
		eventRecord("host-keys", testReceiptURI, market.ComputeEventsVMHostKeysNSID),
		eventRecord("terminated", testReceiptURI, market.ComputeEventsVMTerminatedNSID),
		eventRecord("started", testReceiptURI, market.ComputeEventsVMStartedNSID),
		eventRecord("other-receipt", "at://did:plc:requester/"+market.ReceiptNSID+"/r2", market.ComputeEventsVMOnNetworkNSID),
	)
	creator := &fakeCreator{}
	poller, err := NewPoller(PollerOptions{Lister: lister, Creator: creator})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	created, err := poller.Poll(context.Background(), testBidderDID, testReceiptURI)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if created != 3 || len(creator.syncs) != 3 {
		t.Fatalf("created = %d, syncs = %d, want 3", created, len(creator.syncs))
	}
	if lister.repo != testBidderDID || lister.collection != market.EventNSID {
		t.Fatalf("listed %s/%s", lister.repo, lister.collection)
	}
	wantNames := map[string]bool{}
	for _, rkey := range []string{"on-network", "host-keys", "terminated"} {
		wantNames[InboxName(testReceiptURI, "at://"+testBidderDID+"/"+market.EventNSID+"/"+rkey, testCID)] = true
	}
	for _, sync := range creator.syncs {
		if !wantNames[sync.Name] {
			t.Fatalf("unexpected mailbox %q", sync.Name)
		}
		if sync.APIVersion != v1alpha1.GroupVersion.String() || sync.Kind != contractmarket.KindStateSync {
			t.Fatalf("type meta = %#v", sync.TypeMeta)
		}
		if sync.Labels[inboxLabel] != "true" {
			t.Fatalf("labels = %#v", sync.Labels)
		}
		if sync.Spec.Sink.Adapter != InboxAdapter || sync.Spec.Direction != v1alpha1.StateSyncDirectionOutbound {
			t.Fatalf("spec = %#v", sync.Spec)
		}
		if sync.Spec.ConflictPolicy != v1alpha1.StateSyncConflictSourceWins || sync.Spec.DeletionPolicy != v1alpha1.StateSyncDeletionRetain {
			t.Fatalf("policies = %#v", sync.Spec)
		}
		var payload struct {
			Event v1alpha1.ComputeContractEvent `json:"event"`
		}
		if err := json.Unmarshal(sync.Spec.Payload, &payload); err != nil {
			t.Fatalf("payload: %v", err)
		}
		if payload.Event.URI == "" || payload.Event.CID != testCID || payload.Event.ReceiptURI != testReceiptURI || payload.Event.ReceivedAt == "" {
			t.Fatalf("event = %#v", payload.Event)
		}
		if sync.Spec.IdempotencyKey != "inbox:"+payload.Event.URI+"#"+payload.Event.CID {
			t.Fatalf("idempotency key = %q", sync.Spec.IdempotencyKey)
		}
	}
}

func TestPollerDedupe(t *testing.T) {
	lister := &fakeLister{}
	records := []market.ListedRecord{eventRecord("on-network", testReceiptURI, market.ComputeEventsVMOnNetworkNSID)}
	lister.page(records...)
	creator := &fakeCreator{}
	poller, err := NewPoller(PollerOptions{Lister: lister, Creator: creator})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	if created, err := poller.Poll(context.Background(), testBidderDID, testReceiptURI); err != nil || created != 1 {
		t.Fatalf("first Poll = %d, %v", created, err)
	}
	lister.page(records...)
	if created, err := poller.Poll(context.Background(), testBidderDID, testReceiptURI); err != nil || created != 0 {
		t.Fatalf("second Poll = %d, %v", created, err)
	}
	if creator.calls != 1 {
		t.Fatalf("creator calls = %d, want 1", creator.calls)
	}
}

func TestPollerNoEvents(t *testing.T) {
	lister := &fakeLister{}
	creator := &fakeCreator{}
	poller, err := NewPoller(PollerOptions{Lister: lister, Creator: creator})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	created, err := poller.Poll(context.Background(), testBidderDID, testReceiptURI)
	if err != nil || created != 0 || creator.calls != 0 {
		t.Fatalf("Poll = %d, %v; creator calls = %d", created, err, creator.calls)
	}
}

func TestPollerListError(t *testing.T) {
	lister := &fakeLister{err: errors.New("boom")}
	creator := &fakeCreator{}
	poller, err := NewPoller(PollerOptions{Lister: lister, Creator: creator})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	if _, err := poller.Poll(context.Background(), testBidderDID, testReceiptURI); err == nil {
		t.Fatal("Poll swallowed the list error")
	}
	if creator.calls != 0 {
		t.Fatalf("creator calls = %d", creator.calls)
	}
}

func TestPollerTreatsExistingAsSeen(t *testing.T) {
	lister := &fakeLister{}
	records := []market.ListedRecord{eventRecord("on-network", testReceiptURI, market.ComputeEventsVMOnNetworkNSID)}
	lister.page(records...)
	creator := &fakeCreator{err: contractmarket.ErrExists}
	poller, err := NewPoller(PollerOptions{Lister: lister, Creator: creator})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	if created, err := poller.Poll(context.Background(), testBidderDID, testReceiptURI); err != nil || created != 0 {
		t.Fatalf("first Poll = %d, %v", created, err)
	}
	lister.page(records...)
	if created, err := poller.Poll(context.Background(), testBidderDID, testReceiptURI); err != nil || created != 0 {
		t.Fatalf("second Poll = %d, %v", created, err)
	}
	if creator.calls != 1 {
		t.Fatalf("creator calls = %d, want 1", creator.calls)
	}
}

func TestPollerPaginates(t *testing.T) {
	lister := &fakeLister{pages: []market.ListPage{
		{Records: []market.ListedRecord{eventRecord("a", testReceiptURI, market.ComputeEventsVMOnNetworkNSID)}, Cursor: "next"},
		{Records: []market.ListedRecord{eventRecord("b", testReceiptURI, market.ComputeEventsVMHostKeysNSID)}},
	}}
	creator := &fakeCreator{}
	poller, err := NewPoller(PollerOptions{Lister: lister, Creator: creator})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	if created, err := poller.Poll(context.Background(), testBidderDID, testReceiptURI); err != nil || created != 2 {
		t.Fatalf("Poll = %d, %v", created, err)
	}
	if lister.calls != 2 {
		t.Fatalf("list calls = %d, want 2", lister.calls)
	}
}

func TestPollerSkipsIncompleteRecords(t *testing.T) {
	missingCID := eventRecord("missing-cid", testReceiptURI, market.ComputeEventsVMOnNetworkNSID)
	missingCID.CID = ""
	missingPayload := eventRecord("missing-payload", testReceiptURI, market.ComputeEventsVMOnNetworkNSID)
	delete(missingPayload.Value, "payload")
	missingReceipt := market.ListedRecord{URI: "at://" + testBidderDID + "/" + market.EventNSID + "/missing-receipt", CID: testCID,
		Value: map[string]any{"payload": map[string]any{"uri": "at://" + testBidderDID + "/" + market.ComputeEventsVMOnNetworkNSID + "/p"}}}
	lister := &fakeLister{}
	lister.page(missingCID, missingPayload, missingReceipt)
	creator := &fakeCreator{}
	poller, err := NewPoller(PollerOptions{Lister: lister, Creator: creator})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	if created, err := poller.Poll(context.Background(), testBidderDID, testReceiptURI); err != nil || created != 0 {
		t.Fatalf("Poll = %d, %v", created, err)
	}
}

func TestPollerRequiresTarget(t *testing.T) {
	poller, err := NewPoller(PollerOptions{Lister: &fakeLister{}, Creator: &fakeCreator{}})
	if err != nil {
		t.Fatalf("NewPoller: %v", err)
	}
	if _, err := poller.Poll(context.Background(), "", testReceiptURI); err == nil {
		t.Fatal("empty DID accepted")
	}
	if _, err := poller.Poll(context.Background(), testBidderDID, ""); err == nil {
		t.Fatal("empty receipt accepted")
	}
}

func TestInboxName(t *testing.T) {
	got := InboxName(testReceiptURI, "at://did:plc:bidder/"+market.EventNSID+"/e1", testCID)
	sum := sha256.Sum256([]byte(testReceiptURI + "\x00" + "at://did:plc:bidder/" + market.EventNSID + "/e1" + "#" + testCID))
	want := "inbox-" + hex.EncodeToString(sum[:16])
	if got != want {
		t.Fatalf("InboxName = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, "inbox-") || len(got) != len("inbox-")+32 {
		t.Fatalf("InboxName = %q", got)
	}
	if other := InboxName(testReceiptURI, "at://did:plc:bidder/"+market.EventNSID+"/e2", testCID); other == got {
		t.Fatal("different events share a name")
	}
}

func TestNewPollerRequiresDependencies(t *testing.T) {
	if _, err := NewPoller(PollerOptions{Creator: &fakeCreator{}}); err == nil {
		t.Fatal("poller without a lister")
	}
	if _, err := NewPoller(PollerOptions{Lister: &fakeLister{}}); err == nil {
		t.Fatal("poller without a creator")
	}
}
