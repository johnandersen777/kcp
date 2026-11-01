/*
Copyright 2026 The kcp Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package forwardingregistry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/dynamic"

	dynamiccontext "github.com/kcp-dev/virtual-workspace-framework/pkg/dynamic/context"
)

// forwardApply sends a server-side apply to the backend, so that the backend
// runs the apply itself and records the field manager with operation Apply.
//
// admitted is the object the virtual workspace computed from the apply and
// ran through admission, live the object it was computed from (nil when the
// object does not exist yet).
//
// The body is the client's apply patch with additions that keep the
// guarantees of the Update or Create this replaces:
//   - when live is nil (a create), a fresh random metadata.uid. The backend
//     overwrites the uid of an object it creates, but rejects a changed uid
//     on an existing object (422 on metadata.uid), which forwardApply returns
//     as AlreadyExists, like the Create did. Without it, an object created
//     between the virtual workspace's Get and the apply, possibly one s.Get
//     hides, would be modified by the apply and returned to the caller;
//   - metadata.resourceVersion of live (unless the client set one), so the
//     backend applies onto exactly the object the virtual workspace admitted;
//     a concurrent write fails with a Conflict, which the caller retries with
//     a fresh Get (pinnedResourceVersionConflict). A client resourceVersion
//     other than live's is answered with Conflict without forwarding. If the
//     object is deleted between the Get and the apply, the backend rejects the
//     pinned resourceVersion on its create path; that error is returned as is
//     (fails closed, not retried);
//   - labels that the virtual workspace's mutating admission added (e.g. the
//     APIExport permission claim selector labels) and that the backend would
//     not keep from the apply alone. The applier owns them, as it owns fields
//     set by mutating admission in a kube apiserver.
func forwardApply(
	ctx context.Context,
	delegate dynamic.ResourceInterface,
	groupResource schema.GroupResource,
	name string,
	applyPatch *dynamiccontext.ApplyPatch,
	admitted, live *unstructured.Unstructured,
	options *metav1.UpdateOptions,
	subResources []string,
) (*unstructured.Unstructured, error) {
	createUID := ""
	if live == nil {
		createUID = string(uuid.NewUUID())
	}
	data, pinnedResourceVersion, err := applyPatchBody(groupResource, name, applyPatch.Data, admitted, live, createUID, options.FieldManager, strings.Join(subResources, "/"))
	if err != nil {
		return nil, err
	}

	patchOptions := metav1.PatchOptions{
		DryRun:          options.DryRun,
		FieldManager:    options.FieldManager,
		FieldValidation: options.FieldValidation,
	}
	if applyPatch.Force {
		force := true
		patchOptions.Force = &force
	}

	result, err := delegate.Patch(ctx, name, types.ApplyPatchType, data, patchOptions, subResources...)
	if createUID != "" && isUIDInvalid(err) {
		return nil, apierrors.NewAlreadyExists(groupResource, name)
	}
	if pinnedResourceVersion && apierrors.IsConflict(err) && !isFieldManagerConflict(err) {
		return nil, &pinnedResourceVersionConflict{err: err}
	}
	return result, err
}

// pinnedResourceVersionConflict marks a backend Conflict on the resourceVersion
// forwardApply pinned: the object changed after the virtual workspace read it,
// so the apply is worth retrying with a fresh Get.
type pinnedResourceVersionConflict struct {
	err error
}

func (e *pinnedResourceVersionConflict) Error() string { return e.err.Error() }
func (e *pinnedResourceVersionConflict) Unwrap() error { return e.err }

func isPinnedResourceVersionConflict(err error) bool {
	var conflict *pinnedResourceVersionConflict
	return errors.As(err, &conflict)
}

func unwrapPinnedResourceVersionConflict(err error) error {
	var conflict *pinnedResourceVersionConflict
	if errors.As(err, &conflict) {
		return conflict.err
	}
	return err
}

// isFieldManagerConflict returns whether err is a server-side apply conflict
// between field managers.
func isFieldManagerConflict(err error) bool {
	status, ok := err.(apierrors.APIStatus)
	if !ok || status.Status().Details == nil {
		return false
	}
	for _, cause := range status.Status().Details.Causes {
		if cause.Type == metav1.CauseTypeFieldManagerConflict {
			return true
		}
	}
	return false
}

// isUIDInvalid returns whether err is the backend rejecting a changed metadata.uid.
func isUIDInvalid(err error) bool {
	if !apierrors.IsInvalid(err) {
		return false
	}
	status, ok := err.(apierrors.APIStatus)
	if !ok || status.Status().Details == nil {
		return false
	}
	for _, cause := range status.Status().Details.Causes {
		if cause.Field == "metadata.uid" {
			return true
		}
	}
	return false
}

// applyPatchBody returns the apply patch to send to the backend, see
// forwardApply, and whether it pinned the resourceVersion of live.
func applyPatchBody(groupResource schema.GroupResource, name string, data []byte, admitted, live *unstructured.Unstructured, createUID, manager, subresource string) ([]byte, bool, error) {
	jsonData, err := yaml.YAMLToJSON(data)
	if err != nil {
		return nil, false, apierrors.NewBadRequest(fmt.Sprintf("error decoding apply patch: %v", err))
	}
	decoder := json.NewDecoder(bytes.NewReader(jsonData))
	decoder.UseNumber()
	var patch map[string]interface{}
	if err := decoder.Decode(&patch); err != nil || patch == nil {
		return nil, false, apierrors.NewBadRequest(fmt.Sprintf("error decoding apply patch: %v", err))
	}

	metadata := map[string]interface{}{}
	if m, found := patch["metadata"]; found && m != nil {
		var ok bool
		if metadata, ok = m.(map[string]interface{}); !ok {
			return nil, false, apierrors.NewBadRequest(fmt.Sprintf("apply patch metadata is a %T, not an object", m))
		}
	}

	changed := false
	pinned := false

	if createUID != "" {
		metadata["uid"] = createUID
		changed = true
	}

	if live != nil && live.GetResourceVersion() != "" {
		switch rv, _ := metadata["resourceVersion"].(string); {
		case rv == "":
			metadata["resourceVersion"] = live.GetResourceVersion()
			changed = true
			pinned = true
		case rv != live.GetResourceVersion():
			// The virtual workspace admitted live, not the object at the
			// client's resourceVersion: do not forward.
			return nil, false, apierrors.NewConflict(groupResource, name, errors.New(optimisticLockErrorMsg))
		}
	}

	patchLabels, _ := metadata["labels"].(map[string]interface{})
	for key, value := range admitted.GetLabels() {
		if _, inPatch := patchLabels[key]; inPatch {
			continue
		}
		if live != nil {
			if liveValue, found := live.GetLabels()[key]; found && liveValue == value && labelOwnedByOthers(live, key, manager, subresource) {
				// The backend keeps it without the applier.
				continue
			}
		}
		if patchLabels == nil {
			patchLabels = map[string]interface{}{}
		}
		patchLabels[key] = value
		changed = true
	}

	if !changed {
		return data, pinned, nil
	}
	if patchLabels != nil {
		metadata["labels"] = patchLabels
	}
	patch["metadata"] = metadata
	body, err := json.Marshal(patch)
	return body, pinned, err
}

// optimisticLockErrorMsg is the message of the registry's resourceVersion Conflict.
const optimisticLockErrorMsg = "the object has been modified; please apply your changes to the latest version and try again"

// labelOwnedByOthers returns whether a managedFields entry of obj other than the
// Apply entry of manager on subresource owns the label key.
func labelOwnedByOthers(obj *unstructured.Unstructured, key, manager, subresource string) bool {
	for _, entry := range obj.GetManagedFields() {
		if entry.Manager == manager && entry.Operation == metav1.ManagedFieldsOperationApply && entry.Subresource == subresource {
			continue
		}
		if entry.FieldsV1 == nil {
			continue
		}
		var fields map[string]interface{}
		if err := json.Unmarshal(entry.FieldsV1.Raw, &fields); err != nil {
			continue
		}
		metadata, _ := fields["f:metadata"].(map[string]interface{})
		labels, _ := metadata["f:labels"].(map[string]interface{})
		if _, found := labels["f:"+key]; found {
			return true
		}
	}
	return false
}
