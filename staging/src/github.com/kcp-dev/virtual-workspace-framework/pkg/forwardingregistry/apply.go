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
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
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
// The body is the client's apply patch with two additions that keep the
// guarantees of the Update this replaces:
//   - metadata.resourceVersion of live (unless the client set one), so the
//     backend applies onto exactly the object the virtual workspace admitted;
//     a concurrent write fails with a Conflict, which the caller retries with
//     a fresh Get, as it does for the Update;
//   - labels that the virtual workspace's mutating admission added (e.g. the
//     APIExport permission claim selector labels) and that the backend would
//     not keep from the apply alone. The applier owns them, as it owns fields
//     set by mutating admission in a kube apiserver.
func forwardApply(
	ctx context.Context,
	delegate dynamic.ResourceInterface,
	name string,
	applyPatch *dynamiccontext.ApplyPatch,
	admitted, live *unstructured.Unstructured,
	options *metav1.UpdateOptions,
	subResources []string,
) (*unstructured.Unstructured, error) {
	data, err := applyPatchBody(applyPatch.Data, admitted, live, options.FieldManager, strings.Join(subResources, "/"))
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

	return delegate.Patch(ctx, name, types.ApplyPatchType, data, patchOptions, subResources...)
}

// applyPatchBody returns the apply patch to send to the backend, see forwardApply.
func applyPatchBody(data []byte, admitted, live *unstructured.Unstructured, manager, subresource string) ([]byte, error) {
	jsonData, err := yaml.YAMLToJSON(data)
	if err != nil {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("error decoding apply patch: %v", err))
	}
	decoder := json.NewDecoder(bytes.NewReader(jsonData))
	decoder.UseNumber()
	var patch map[string]interface{}
	if err := decoder.Decode(&patch); err != nil || patch == nil {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("error decoding apply patch: %v", err))
	}

	metadata := map[string]interface{}{}
	if m, found := patch["metadata"]; found && m != nil {
		var ok bool
		if metadata, ok = m.(map[string]interface{}); !ok {
			return nil, apierrors.NewBadRequest(fmt.Sprintf("apply patch metadata is a %T, not an object", m))
		}
	}

	changed := false

	if live != nil && live.GetResourceVersion() != "" {
		if rv, _ := metadata["resourceVersion"].(string); rv == "" {
			metadata["resourceVersion"] = live.GetResourceVersion()
			changed = true
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
		return data, nil
	}
	if patchLabels != nil {
		metadata["labels"] = patchLabels
	}
	patch["metadata"] = metadata
	return json.Marshal(patch)
}

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
