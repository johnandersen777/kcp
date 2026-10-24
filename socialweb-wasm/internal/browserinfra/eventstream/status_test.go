package eventstream

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	eventsv1 "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
)

func readyEventSource(name string, generation int64) *eventsv1.EventSource {
	source := &eventsv1.EventSource{ObjectMeta: metav1.ObjectMeta{Name: name, Generation: generation}}
	source.Status.Conditions = []metav1.Condition{
		{Type: eventsv1.EventSourceConditionBackendBound, Status: metav1.ConditionTrue, Reason: ReasonBound, ObservedGeneration: generation},
		{Type: eventsv1.EventSourceConditionReady, Status: metav1.ConditionTrue, Reason: ReasonConfigured, ObservedGeneration: generation},
	}
	return source
}

func readySink() eventsv1.EventStreamSink {
	return eventsv1.EventStreamSink{Kind: eventsv1.EventStreamSinkHandler, Name: "market-handler"}
}

func eventStream(generation int64) *eventsv1.EventStream {
	return &eventsv1.EventStream{ObjectMeta: metav1.ObjectMeta{Name: "market", Generation: generation},
		Spec: eventsv1.EventStreamSpec{
			SourceRef: commonv1.ObjectRef{Name: "source"},
			Sink:      readySink(),
		}}
}

func requireCondition(t *testing.T, conditions []metav1.Condition, kind string, status metav1.ConditionStatus, reason string, generation int64) {
	t.Helper()
	cond := meta.FindStatusCondition(conditions, kind)
	if cond == nil {
		t.Fatalf("%s condition missing: %#v", kind, conditions)
	}
	if cond.Status != status || cond.Reason != reason || cond.ObservedGeneration != generation {
		t.Fatalf("%s = %#v, want status %s reason %q generation %d", kind, cond, status, reason, generation)
	}
}

func TestEventStreamStatus(t *testing.T) {
	t.Run("missing source ref fails", func(t *testing.T) {
		stream := eventStream(3)
		stream.Spec.SourceRef = commonv1.ObjectRef{}
		st := EventStreamStatus(stream, "ws", nil)
		if st.Phase != eventsv1.EventStreamPhaseFailed || st.ObservedGeneration != 3 {
			t.Fatalf("status = %#v", st)
		}
		requireCondition(t, st.Conditions, eventsv1.EventStreamConditionReady, metav1.ConditionFalse, ReasonInvalidSpec, 3)
		requireCondition(t, st.Conditions, eventsv1.EventStreamConditionFailed, metav1.ConditionTrue, ReasonInvalidSpec, 3)
	})

	t.Run("suspended", func(t *testing.T) {
		stream := eventStream(2)
		stream.Spec.Suspend = true
		st := EventStreamStatus(stream, "ws", nil)
		if st.Phase != eventsv1.EventStreamPhaseSuspended {
			t.Fatalf("phase = %q", st.Phase)
		}
		requireCondition(t, st.Conditions, eventsv1.EventStreamConditionReady, metav1.ConditionFalse, ReasonSuspended, 2)
		requireCondition(t, st.Conditions, eventsv1.EventStreamConditionFailed, metav1.ConditionFalse, "NoError", 2)
	})

	t.Run("invalid sink", func(t *testing.T) {
		stream := eventStream(4)
		stream.Spec.Sink.Kind = "mystery"
		st := EventStreamStatus(stream, "ws", readyEventSource("source", 1))
		if st.Phase != eventsv1.EventStreamPhaseFailed {
			t.Fatalf("phase = %q", st.Phase)
		}
		requireCondition(t, st.Conditions, eventsv1.EventStreamConditionReady, metav1.ConditionFalse, ReasonInvalidSpec, 4)
	})

	t.Run("missing source", func(t *testing.T) {
		st := EventStreamStatus(eventStream(1), "ws", nil)
		if st.Phase != eventsv1.EventStreamPhasePending {
			t.Fatalf("phase = %q", st.Phase)
		}
		requireCondition(t, st.Conditions, eventsv1.EventStreamConditionReady, metav1.ConditionFalse, ReasonSourceMissing, 1)
	})

	t.Run("unbound source", func(t *testing.T) {
		source := &eventsv1.EventSource{ObjectMeta: metav1.ObjectMeta{Name: "source", Generation: 2}}
		st := EventStreamStatus(eventStream(1), "ws", source)
		if st.Phase != eventsv1.EventStreamPhasePending {
			t.Fatalf("phase = %q", st.Phase)
		}
		requireCondition(t, st.Conditions, eventsv1.EventStreamConditionReady, metav1.ConditionFalse, ReasonSourceUnbound, 1)
	})

	t.Run("ready", func(t *testing.T) {
		stream := eventStream(5)
		at := metav1.Now()
		stream.Status.LastEventAt = &at
		stream.Status.Cursor = 12
		st := EventStreamStatus(stream, "ws", readyEventSource("source", 3))
		if st.Phase != eventsv1.EventStreamPhaseRunning || st.ObservedGeneration != 5 || st.Cursor != 12 || st.Lag != 0 {
			t.Fatalf("status = %#v", st)
		}
		if st.LastEventAt == nil || !st.LastEventAt.Equal(&at) {
			t.Fatalf("lastEventAt = %#v", st.LastEventAt)
		}
		requireCondition(t, st.Conditions, eventsv1.EventStreamConditionReady, metav1.ConditionTrue, ReasonStreamOpen, 5)
		requireCondition(t, st.Conditions, eventsv1.EventStreamConditionLagging, metav1.ConditionFalse, "UpToDate", 5)
		requireCondition(t, st.Conditions, eventsv1.EventStreamConditionFailed, metav1.ConditionFalse, "NoError", 5)
	})

	t.Run("stale source status is not ready", func(t *testing.T) {
		source := readyEventSource("source", 3)
		for i := range source.Status.Conditions {
			source.Status.Conditions[i].ObservedGeneration = 2
		}
		st := EventStreamStatus(eventStream(1), "ws", source)
		if st.Phase != eventsv1.EventStreamPhasePending {
			t.Fatalf("phase = %q", st.Phase)
		}
		requireCondition(t, st.Conditions, eventsv1.EventStreamConditionReady, metav1.ConditionFalse, ReasonSourceUnbound, 1)
	})

	t.Run("cross workspace source", func(t *testing.T) {
		stream := eventStream(1)
		stream.Spec.SourceRef.Workspace = "other"
		st := EventStreamStatus(stream, "ws", nil)
		requireCondition(t, st.Conditions, eventsv1.EventStreamConditionReady, metav1.ConditionFalse, ReasonSourceUnbound, 1)
	})
}

func TestEventSourceStatus(t *testing.T) {
	source := &eventsv1.EventSource{ObjectMeta: metav1.ObjectMeta{Name: "source", Generation: 4},
		Spec: eventsv1.EventSourceSpec{BackendRef: commonv1.ObjectRef{Kind: eventsv1.PdsEventSourceKind, Name: "repo"}}}

	t.Run("not a contract kind", func(t *testing.T) {
		bad := source.DeepCopy()
		bad.Spec.BackendRef.Kind = "Vault"
		st := EventSourceStatus(bad, "ws", nil, 0)
		requireCondition(t, st.Conditions, eventsv1.EventSourceConditionBackendBound, metav1.ConditionFalse, ReasonNotAContractKind, 4)
		requireCondition(t, st.Conditions, eventsv1.EventSourceConditionReady, metav1.ConditionFalse, ReasonNotAContractKind, 4)
	})

	t.Run("cross workspace", func(t *testing.T) {
		bad := source.DeepCopy()
		bad.Spec.BackendRef.Workspace = "other"
		st := EventSourceStatus(bad, "ws", nil, 0)
		requireCondition(t, st.Conditions, eventsv1.EventSourceConditionBackendBound, metav1.ConditionFalse, ReasonCrossWorkspace, 4)
	})

	t.Run("missing backend", func(t *testing.T) {
		st := EventSourceStatus(source, "ws", nil, 0)
		requireCondition(t, st.Conditions, eventsv1.EventSourceConditionBackendBound, metav1.ConditionFalse, ReasonBackendMissing, 4)
		requireCondition(t, st.Conditions, eventsv1.EventSourceConditionReady, metav1.ConditionFalse, ReasonBackendMissing, 4)
	})

	t.Run("backend not ready", func(t *testing.T) {
		backend := &eventsv1.EventSourceStatus{ObservedGeneration: 2, Address: "at://repo"}
		st := EventSourceStatus(source, "ws", backend, 2)
		requireCondition(t, st.Conditions, eventsv1.EventSourceConditionBackendBound, metav1.ConditionTrue, ReasonBound, 4)
		requireCondition(t, st.Conditions, eventsv1.EventSourceConditionReady, metav1.ConditionFalse, ReasonBackendNotReady, 4)
		if st.Address != "at://repo" {
			t.Fatalf("address = %q", st.Address)
		}
	})

	t.Run("ready backend is copied", func(t *testing.T) {
		at := metav1.Now()
		backend := &eventsv1.EventSourceStatus{ObservedGeneration: 2, Cursor: 9, Lag: 1, Address: "at://repo", LastEventAt: &at}
		backend.Conditions = []metav1.Condition{{Type: eventsv1.EventSourceConditionReady, Status: metav1.ConditionTrue,
			Reason: ReasonConfigured, Message: "configured", ObservedGeneration: 2}}
		st := EventSourceStatus(source, "ws", backend, 2)
		if st.Cursor != 9 || st.Lag != 1 || st.Address != "at://repo" || st.LastEventAt == nil {
			t.Fatalf("status = %#v", st)
		}
		requireCondition(t, st.Conditions, eventsv1.EventSourceConditionBackendBound, metav1.ConditionTrue, ReasonBound, 4)
		requireCondition(t, st.Conditions, eventsv1.EventSourceConditionReady, metav1.ConditionTrue, ReasonConfigured, 4)
	})

	t.Run("stale backend is not ready", func(t *testing.T) {
		backend := &eventsv1.EventSourceStatus{Conditions: []metav1.Condition{{Type: eventsv1.EventSourceConditionReady,
			Status: metav1.ConditionTrue, Reason: ReasonConfigured, ObservedGeneration: 1}}}
		st := EventSourceStatus(source, "ws", backend, 3)
		// The real controller keeps the backend's reason but flips the status.
		requireCondition(t, st.Conditions, eventsv1.EventSourceConditionReady, metav1.ConditionFalse, ReasonConfigured, 4)
	})
}

func TestBackendStatuses(t *testing.T) {
	t.Run("pds ready", func(t *testing.T) {
		source := &eventsv1.PdsEventSource{ObjectMeta: metav1.ObjectMeta{Generation: 3},
			Spec: eventsv1.PdsEventSourceSpec{RepoRef: commonv1.ObjectRef{Kind: AtprotoRepoKind, Name: "repo-did"}}}
		st := PdsSourceStatus(source, "ws")
		if st.ObservedGeneration != 3 || !strings.Contains(st.Address, "repo-did") || !strings.Contains(st.Address, "ws:") {
			t.Fatalf("status = %#v", st)
		}
		requireCondition(t, st.Conditions, eventsv1.EventSourceConditionReady, metav1.ConditionTrue, ReasonConfigured, 3)
	})

	t.Run("pds invalid repo", func(t *testing.T) {
		source := &eventsv1.PdsEventSource{ObjectMeta: metav1.ObjectMeta{Generation: 1},
			Spec: eventsv1.PdsEventSourceSpec{RepoRef: commonv1.ObjectRef{Kind: "Vault", Name: "repo"}}}
		st := PdsSourceStatus(source, "ws")
		if st.Address != "" {
			t.Fatalf("address = %q, want empty", st.Address)
		}
		requireCondition(t, st.Conditions, eventsv1.EventSourceConditionReady, metav1.ConditionFalse, ReasonInvalidSpec, 1)
	})

	t.Run("relay url", func(t *testing.T) {
		source := &eventsv1.RelayEventSource{ObjectMeta: metav1.ObjectMeta{Generation: 2},
			Spec: eventsv1.RelayEventSourceSpec{URL: "wss://relay.example/subscribe"}}
		st := RelaySourceStatus(source)
		if st.Address != "wss://relay.example/subscribe" {
			t.Fatalf("address = %q", st.Address)
		}
		requireCondition(t, st.Conditions, eventsv1.EventSourceConditionReady, metav1.ConditionTrue, ReasonConfigured, 2)
	})

	t.Run("relay ref", func(t *testing.T) {
		source := &eventsv1.RelayEventSource{ObjectMeta: metav1.ObjectMeta{Generation: 2},
			Spec: eventsv1.RelayEventSourceSpec{RelayRef: &commonv1.ObjectRef{Name: "R1"}}}
		st := RelaySourceStatus(source)
		if st.Address != "http://relay.r1" {
			t.Fatalf("address = %q", st.Address)
		}
	})

	t.Run("relay invalid spec", func(t *testing.T) {
		for name, spec := range map[string]eventsv1.RelayEventSourceSpec{
			"both":         {URL: "wss://relay.example", RelayRef: &commonv1.ObjectRef{Name: "r"}},
			"neither":      {},
			"nameless ref": {RelayRef: &commonv1.ObjectRef{}},
		} {
			t.Run(name, func(t *testing.T) {
				source := &eventsv1.RelayEventSource{ObjectMeta: metav1.ObjectMeta{Generation: 1}, Spec: spec}
				st := RelaySourceStatus(source)
				requireCondition(t, st.Conditions, eventsv1.EventSourceConditionReady, metav1.ConditionFalse, ReasonInvalidSpec, 1)
			})
		}
	})

	t.Run("jetstream", func(t *testing.T) {
		source := &eventsv1.JetstreamEventSource{ObjectMeta: metav1.ObjectMeta{Generation: 6},
			Spec: eventsv1.JetstreamEventSourceSpec{URL: "wss://jetstream.example/subscribe"}}
		st := JetstreamSourceStatus(source)
		if st.Address != "wss://jetstream.example/subscribe" || st.ObservedGeneration != 6 {
			t.Fatalf("status = %#v", st)
		}
		requireCondition(t, st.Conditions, eventsv1.EventSourceConditionReady, metav1.ConditionTrue, ReasonConfigured, 6)

		empty := &eventsv1.JetstreamEventSource{ObjectMeta: metav1.ObjectMeta{Generation: 1}}
		st = JetstreamSourceStatus(empty)
		requireCondition(t, st.Conditions, eventsv1.EventSourceConditionReady, metav1.ConditionFalse, ReasonInvalidSpec, 1)
	})
}

func TestBackendRef(t *testing.T) {
	ref, err := BackendRef("ws", commonv1.ObjectRef{Kind: "pdseventsource", Name: " repo "})
	if err != nil || ref.Kind != eventsv1.PdsEventSourceKind || ref.Name != "repo" || ref.Group != eventsv1.GroupName || ref.Workspace != "ws" {
		t.Fatalf("ref = %#v, err = %v", ref, err)
	}
	if _, err := BackendRef("ws", commonv1.ObjectRef{Kind: "EventSource", Name: "x"}); err == nil {
		t.Fatal("EventSource is not a backend kind")
	}
	if _, err := BackendRef("ws", commonv1.ObjectRef{Kind: eventsv1.PdsEventSourceKind, Name: "x", Workspace: "other"}); err == nil {
		t.Fatal("cross-workspace backend accepted")
	}
	if _, err := BackendRef("ws", commonv1.ObjectRef{Group: "elsewhere", Kind: eventsv1.PdsEventSourceKind, Name: "x"}); err == nil {
		t.Fatal("foreign group accepted")
	}
}

func TestRegisterRejectsNilManager(t *testing.T) {
	if _, err := Register(nil, Options{}); err == nil {
		t.Fatal("Register(nil) succeeded")
	}
}
