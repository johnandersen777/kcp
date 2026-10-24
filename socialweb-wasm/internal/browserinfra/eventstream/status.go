package eventstream

import (
	"cmp"
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	eventsv1 "github.com/johnandersen777/socialweb-computer-kcp/api/events/v1alpha1"
	v1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/kit/conds"
)

const (
	// Export is the APIExport the real EventStream and EventSource controllers
	// serve.
	Export = "events"

	// AtprotoRepoKind is the only repoRef kind a PdsEventSource accepts.
	AtprotoRepoKind = "AtprotoRepo"

	// DefaultRelayKind is the relayRef kind used when none is named.
	DefaultRelayKind = "Relay"

	// Reasons match internal/controllers/eventstream and
	// internal/controllers/eventsource so consumers of the conditions see the
	// same vocabulary.
	ReasonBound = "BackendBound"

	ReasonConfigured = "Configured"

	ReasonStreamOpen = "StreamOpen"

	ReasonInvalidSpec = "InvalidSpec"

	ReasonSuspended = "Suspended"

	ReasonSourceMissing = "SourceMissing"

	ReasonSourceUnbound = "SourceUnbound"

	ReasonNotAContractKind = "NotAContractKind"

	ReasonCrossWorkspace = "CrossWorkspace"

	ReasonBackendMissing = "BackendMissing"

	ReasonBackendNotReady = "BackendNotReady"
)

var (
	errNotAContractKind = errors.New("not a kind of the eventsources contract")

	errCrossWorkspace = errors.New("the backend is outside the EventSource's workspace")
)

// EventStreamStatus is the subscriber's whole status. A stream is Ready once
// its EventSource is bound to a Ready backend; the browser poller then stands
// in for the firehose, so the cursor and lag stay put.
func EventStreamStatus(stream *eventsv1.EventStream, workspace string, source *eventsv1.EventSource) eventsv1.EventStreamStatus {
	status := *stream.Status.DeepCopy()
	status.ObservedGeneration = stream.Generation
	status.Lag = 0
	sinkErr := sinkRefError(stream.Spec.Sink)
	sourceRef := commonv1.ObjectRef{Group: eventsv1.GroupName, Kind: eventsv1.EventSourceKind, Name: strings.TrimSpace(stream.Spec.SourceRef.Name),
		Workspace: stream.Spec.SourceRef.InWorkspace(workspace)}
	switch {
	case sourceRef.Name == "":
		status.Phase = eventsv1.EventStreamPhaseFailed
		streamConditions(&status, stream.Generation, false, false, true, ReasonInvalidSpec, "spec.sourceRef needs a name")
	case stream.Spec.Suspend:
		status.Phase = eventsv1.EventStreamPhaseSuspended
		streamConditions(&status, stream.Generation, false, false, false, ReasonSuspended, "suspended")
	case sinkErr != nil:
		status.Phase = eventsv1.EventStreamPhaseFailed
		streamConditions(&status, stream.Generation, false, false, true, ReasonInvalidSpec, sinkErr.Error())
	case sourceRef.Workspace != workspace:
		status.Phase = eventsv1.EventStreamPhasePending
		streamConditions(&status, stream.Generation, false, false, false, ReasonSourceUnbound, "the EventSource is in another workspace")
	case source == nil:
		status.Phase = eventsv1.EventStreamPhasePending
		streamConditions(&status, stream.Generation, false, false, false, ReasonSourceMissing,
			"the EventSource "+sourceRef.Name+" does not exist")
	case !sourceReady(source):
		status.Phase = eventsv1.EventStreamPhasePending
		streamConditions(&status, stream.Generation, false, false, false, ReasonSourceUnbound,
			"the EventSource "+sourceRef.Name+" is not bound to a Ready backend")
	default:
		status.Phase = eventsv1.EventStreamPhaseRunning
		streamConditions(&status, stream.Generation, true, false, false, ReasonStreamOpen, "the in-browser poller serves the stream")
	}
	return status
}

func sinkRefError(sink eventsv1.EventStreamSink) error {
	if strings.TrimSpace(sink.Name) == "" {
		return errors.New("spec.sink needs a name")
	}
	switch sink.Kind {
	case eventsv1.EventStreamSinkRecordWatch, eventsv1.EventStreamSinkHandler:
		return nil
	}
	return fmt.Errorf("unknown sink kind %q", sink.Kind)
}

func sourceReady(source *eventsv1.EventSource) bool {
	if meta.FindStatusCondition(source.Status.Conditions, eventsv1.EventSourceConditionBackendBound) == nil ||
		!conditionTrueAt(source.Status.Conditions, eventsv1.EventSourceConditionBackendBound, source.Generation) {
		return false
	}
	return conditionTrueAt(source.Status.Conditions, eventsv1.EventSourceConditionReady, source.Generation)
}

func conditionTrueAt(conditions []metav1.Condition, kind string, generation int64) bool {
	c := meta.FindStatusCondition(conditions, kind)
	return c != nil && c.Status == metav1.ConditionTrue && c.ObservedGeneration == generation
}

func streamConditions(status *eventsv1.EventStreamStatus, generation int64, ready, lagging, failed bool, reason, message string) {
	readyReason := reason
	if ready {
		readyReason = ReasonStreamOpen
	}
	conds.Set(&status.Conditions, generation, eventsv1.EventStreamConditionReady, ready, readyReason, message)
	lagReason, lagMessage := "UpToDate", "the stream is current"
	if lagging {
		lagReason, lagMessage = "CursorGap", fmt.Sprintf("%d events behind", status.Lag)
	}
	conds.Set(&status.Conditions, generation, eventsv1.EventStreamConditionLagging, lagging, lagReason, lagMessage)
	failedReason, failedMessage := "NoError", "the stream is healthy"
	if failed {
		failedReason, failedMessage = reason, message
	}
	conds.Set(&status.Conditions, generation, eventsv1.EventStreamConditionFailed, failed, failedReason, failedMessage)
}

// BackendRef normalizes and validates an EventSource backendRef. The browser
// binds only the three eventsources-contract kinds in the same workspace.
func BackendRef(workspace string, ref commonv1.ObjectRef) (commonv1.ObjectRef, error) {
	kind := canonicalBackendKind(ref.Kind)
	if kind == "" {
		return commonv1.ObjectRef{}, errNotAContractKind
	}
	if g := strings.TrimSpace(ref.Group); g != "" && g != eventsv1.GroupName {
		return commonv1.ObjectRef{}, errNotAContractKind
	}
	out := commonv1.ObjectRef{Group: eventsv1.GroupName, Kind: kind, Name: strings.TrimSpace(ref.Name), Workspace: ref.InWorkspace(workspace)}
	if out.Workspace != workspace {
		return out, errCrossWorkspace
	}
	return out, nil
}

func canonicalBackendKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case strings.ToLower(eventsv1.PdsEventSourceKind):
		return eventsv1.PdsEventSourceKind
	case strings.ToLower(eventsv1.RelayEventSourceKind):
		return eventsv1.RelayEventSourceKind
	case strings.ToLower(eventsv1.JetstreamEventSourceKind):
		return eventsv1.JetstreamEventSourceKind
	}
	return ""
}

// EventSourceStatus mirrors internal/controllers/eventsource: BackendBound
// tracks the backend object, and Ready and the cursor fields are copied from
// the backend at its generation. A nil backend means the object is missing.
func EventSourceStatus(source *eventsv1.EventSource, workspace string, backend *eventsv1.EventSourceStatus, backendGeneration int64) eventsv1.EventSourceStatus {
	status := *source.Status.DeepCopy()
	generation := source.Generation
	status.ObservedGeneration = generation
	ref, err := BackendRef(workspace, source.Spec.BackendRef)
	switch {
	case errors.Is(err, errNotAContractKind):
		conds.Set(&status.Conditions, generation, eventsv1.EventSourceConditionBackendBound, false, ReasonNotAContractKind,
			fmt.Sprintf("%s is not a kind of the eventsources contract", source.Spec.BackendRef.String()))
		conds.Set(&status.Conditions, generation, eventsv1.EventSourceConditionReady, false, ReasonNotAContractKind, "the backend is not bound")
	case errors.Is(err, errCrossWorkspace):
		conds.Set(&status.Conditions, generation, eventsv1.EventSourceConditionBackendBound, false, ReasonCrossWorkspace,
			fmt.Sprintf("the backend %s is not in the workspace of the EventSource", source.Spec.BackendRef.String()))
		conds.Set(&status.Conditions, generation, eventsv1.EventSourceConditionReady, false, ReasonCrossWorkspace, "the backend is not bound")
	case backend == nil:
		conds.Set(&status.Conditions, generation, eventsv1.EventSourceConditionBackendBound, false, ReasonBackendMissing,
			fmt.Sprintf("the backend %s does not exist", ref.String()))
		conds.Set(&status.Conditions, generation, eventsv1.EventSourceConditionReady, false, ReasonBackendMissing, "the backend is not bound")
	default:
		conds.Set(&status.Conditions, generation, eventsv1.EventSourceConditionBackendBound, true, ReasonBound, "bound to "+ref.String())
		status.Cursor, status.LastEventAt, status.Lag, status.Address = backend.Cursor, backend.LastEventAt, backend.Lag, backend.Address
		ready := meta.FindStatusCondition(backend.Conditions, eventsv1.EventSourceConditionReady)
		switch {
		case ready != nil && ready.Status == metav1.ConditionTrue && ready.ObservedGeneration == backendGeneration:
			conds.Set(&status.Conditions, generation, eventsv1.EventSourceConditionReady, true, ready.Reason, ready.Message)
		case ready != nil:
			conds.Set(&status.Conditions, generation, eventsv1.EventSourceConditionReady, false,
				cmp.Or(ready.Reason, ReasonBackendNotReady), ready.Message)
		default:
			conds.Set(&status.Conditions, generation, eventsv1.EventSourceConditionReady, false, ReasonBackendNotReady,
				"the backend reports no Ready condition")
		}
	}
	return status
}

// PdsSourceStatus reports a PdsEventSource Ready; no PDS connection is opened.
func PdsSourceStatus(source *eventsv1.PdsEventSource, workspace string) eventsv1.EventSourceStatus {
	address, err := repoAddress(workspace, source.Spec.RepoRef)
	return backendStatus(source.Status, source.Generation, address, err)
}

func repoAddress(workspace string, ref commonv1.ObjectRef) (string, error) {
	if !strings.EqualFold(strings.TrimSpace(ref.Kind), AtprotoRepoKind) {
		return "", fmt.Errorf("the PdsEventSource repoRef must name an AtprotoRepo, not %q", ref.Kind)
	}
	if g := strings.TrimSpace(ref.Group); g != "" && g != v1alpha1.GroupName {
		return "", fmt.Errorf("the PdsEventSource repoRef group %q is not %s", g, v1alpha1.GroupName)
	}
	name := strings.TrimSpace(ref.Name)
	if name == "" {
		return "", errors.New("the PdsEventSource repoRef needs a name")
	}
	return commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: AtprotoRepoKind, Name: name,
		Workspace: ref.InWorkspace(workspace)}.String(), nil
}

// RelaySourceStatus reports a RelayEventSource Ready; the relay URL is never
// dialed.
func RelaySourceStatus(source *eventsv1.RelayEventSource) eventsv1.EventSourceStatus {
	address, err := relayAddress(source.Spec)
	return backendStatus(source.Status, source.Generation, address, err)
}

func relayAddress(spec eventsv1.RelayEventSourceSpec) (string, error) {
	raw := strings.TrimSpace(spec.URL)
	switch {
	case spec.RelayRef != nil && raw != "":
		return "", errors.New("the RelayEventSource names both relayRef and url")
	case spec.RelayRef != nil:
		ref := *spec.RelayRef
		kind := strings.ToLower(cmp.Or(strings.TrimSpace(ref.Kind), DefaultRelayKind))
		name := strings.ToLower(strings.TrimSpace(ref.Name))
		if name == "" {
			return "", errors.New("the RelayEventSource relayRef needs a name")
		}
		// ponytail: one address form; the real controller builds a kcp service
		// FQDN for a cross-workspace relayRef.
		return "http://" + kind + "." + name, nil
	case raw != "":
		return raw, nil
	default:
		return "", errors.New("the RelayEventSource needs relayRef or url")
	}
}

// JetstreamSourceStatus reports a JetstreamEventSource Ready; no WebSocket is
// opened.
func JetstreamSourceStatus(source *eventsv1.JetstreamEventSource) eventsv1.EventSourceStatus {
	address := strings.TrimSpace(source.Spec.URL)
	var err error
	if address == "" {
		err = errors.New("the JetstreamEventSource needs a url")
	}
	return backendStatus(source.Status, source.Generation, address, err)
}

func backendStatus(current eventsv1.EventSourceStatus, generation int64, address string, err error) eventsv1.EventSourceStatus {
	status := *current.DeepCopy()
	status.ObservedGeneration = generation
	status.Lag = 0
	if err != nil {
		status.Address = ""
		conds.Set(&status.Conditions, generation, eventsv1.EventSourceConditionReady, false, ReasonInvalidSpec, err.Error())
		return status
	}
	status.Address = address
	conds.Set(&status.Conditions, generation, eventsv1.EventSourceConditionReady, true, ReasonConfigured, "the source subscribes to "+address)
	return status
}
