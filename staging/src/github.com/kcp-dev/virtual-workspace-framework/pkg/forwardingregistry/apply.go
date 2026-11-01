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

	"sigs.k8s.io/yaml"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/util/csaupgrade"
	"k8s.io/klog/v2"

	dynamiccontext "github.com/kcp-dev/virtual-workspace-framework/pkg/dynamic/context"
)

// forwardApply sends a server-side apply to the backend, so that the backend
// runs the apply itself and records the field manager with operation Apply.
//
// live is the existing object the virtual workspace computed the apply from
// and ran admission on. Creates go through createAsApply instead.
//
// The body is the client's apply patch with additions that keep the
// guarantees of the Update this replaces:
//   - metadata.resourceVersion of live (unless the client set one), so the
//     backend applies onto exactly the object the virtual workspace admitted;
//     a concurrent write fails with a Conflict, which the caller retries with
//     a fresh Get (pinnedResourceVersionConflict). A client resourceVersion
//     other than live's is answered with Conflict without forwarding. If the
//     object is deleted between the Get and the apply, the backend rejects the
//     pinned resourceVersion on its create path; that error is returned as is
//     (fails closed, not retried);
//   - what the virtual workspace's mutating admission changed, as recorded by
//     apiserver.TrackApplyAdmission (see admissionChangesFrom): labels and
//     annotations it added or changed (e.g. the APIExport permission claim
//     selector labels) and finalizers it added. The applier owns them, as it
//     owns fields set by mutating admission in a kube apiserver. Nothing
//     admission did not touch is added, so the applier does not adopt fields
//     it never set. Any other change fails the request: forwarding the apply
//     without it would store something other than what validation admitted.
func forwardApply(
	ctx context.Context,
	delegate dynamic.ResourceInterface,
	groupResource schema.GroupResource,
	name string,
	applyPatch *dynamiccontext.ApplyPatch,
	live *unstructured.Unstructured,
	options *metav1.UpdateOptions,
	subResources []string,
) (*unstructured.Unstructured, error) {
	changes, err := admissionChangesFrom(applyPatch)
	if err != nil {
		return nil, err
	}
	data, pinnedResourceVersion, err := applyPatchBody(groupResource, name, applyPatch.Data, live, changes)
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

// createAsApply creates the object a server-side apply computed in the virtual
// workspace (admitted, after admission) and then records its field manager as
// an Apply manager.
//
// The Create is atomic: it fails with AlreadyExists if the object exists by
// now, including one the storage hides (e.g. a label selector wrapper). A
// forwarded apply cannot do that, it would apply onto the existing object;
// and the backend rejects an apply that creates and carries a uid (409 uid
// mismatch) or a resourceVersion.
//
// The backend records the creating manager with operation Update, so a later
// apply omitting a field would not remove it. Unless this is a dry run, a JSON
// patch pinned to the created resourceVersion then turns that Update entry
// into the manager's Apply entry (client-go csaupgrade). If that fails, e.g.
// on a concurrent write, the created object is returned anyway: the object
// then keeps the Update entry, as before apply forwarding.
func createAsApply(
	ctx context.Context,
	delegate dynamic.ResourceInterface,
	admitted *unstructured.Unstructured,
	options *metav1.UpdateOptions,
) (*unstructured.Unstructured, error) {
	created, err := delegate.Create(ctx, admitted, updateToCreateOptions(options))
	if err != nil || len(options.DryRun) > 0 {
		return created, err
	}

	manager := options.FieldManager
	upgrade, err := csaupgrade.UpgradeManagedFieldsPatch(created, sets.New(manager), manager)
	if err != nil || len(upgrade) == 0 {
		if err != nil {
			klog.FromContext(ctx).V(2).Info("not upgrading the managed fields of a created object", "name", created.GetName(), "manager", manager, "err", err)
		}
		return created, nil
	}
	upgraded, err := delegate.Patch(ctx, created.GetName(), types.JSONPatchType, upgrade, metav1.PatchOptions{})
	if err != nil {
		klog.FromContext(ctx).V(2).Info("could not upgrade the managed fields of a created object", "name", created.GetName(), "manager", manager, "err", err)
		return created, nil
	}
	return upgraded, nil
}

// applyPatchBody returns the apply patch to send to the backend, see
// forwardApply, and whether it pinned the resourceVersion of live.
func applyPatchBody(groupResource schema.GroupResource, name string, data []byte, live *unstructured.Unstructured, admission *admissionChanges) ([]byte, bool, error) {
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

	if admission != nil {
		for field, values := range map[string]map[string]string{"labels": admission.labels, "annotations": admission.annotations} {
			if len(values) == 0 {
				continue
			}
			patchValues, _ := metadata[field].(map[string]interface{})
			if patchValues == nil {
				patchValues = map[string]interface{}{}
			}
			for key, value := range values {
				patchValues[key] = value
			}
			metadata[field] = patchValues
			changed = true
		}
		if len(admission.finalizers) > 0 {
			patchFinalizers, _ := metadata["finalizers"].([]interface{})
			present := sets.New[string]()
			for _, f := range patchFinalizers {
				if s, ok := f.(string); ok {
					present.Insert(s)
				}
			}
			for _, f := range admission.finalizers {
				if !present.Has(f) {
					patchFinalizers = append(patchFinalizers, f)
				}
			}
			metadata["finalizers"] = patchFinalizers
			changed = true
		}
	}

	if !changed {
		return data, pinned, nil
	}
	patch["metadata"] = metadata
	body, err := json.Marshal(patch)
	return body, pinned, err
}

// optimisticLockErrorMsg is the message of the registry's resourceVersion Conflict.
const optimisticLockErrorMsg = "the object has been modified; please apply your changes to the latest version and try again"

// admissionChanges is what mutating admission changed that a server-side apply
// can carry: added or changed labels and annotations, added finalizers. These
// are ObjectMeta fields with a known merge type (granular string maps and a
// set), so adding them to the apply sets exactly them.
type admissionChanges struct {
	labels      map[string]string
	annotations map[string]string
	finalizers  []string
}

// admissionChangesFrom returns what mutating admission changed in the current
// attempt, as recorded by apiserver.TrackApplyAdmission, or an InternalError
// if it changed anything else. Without the schema the virtual workspace
// storage cannot tell whether a map or list elsewhere is atomic or granular,
// so it cannot express those changes in an apply safely; removals cannot be
// expressed at all (an apply only removes fields its manager owned).
func admissionChangesFrom(applyPatch *dynamiccontext.ApplyPatch) (*admissionChanges, error) {
	before, after, mutated := applyPatch.Admission()
	if !mutated {
		return nil, nil
	}
	beforeObj, ok := before.(*unstructured.Unstructured)
	if !ok {
		return nil, fmt.Errorf("not an Unstructured: %T", before)
	}
	afterObj, ok := after.(*unstructured.Unstructured)
	if !ok {
		return nil, fmt.Errorf("not an Unstructured: %T", after)
	}
	cannotForward := func(what string) error {
		return apierrors.NewInternalError(fmt.Errorf("mutating admission %s, which cannot be forwarded with the server-side apply", what))
	}

	changes := &admissionChanges{}
	var err error
	if changes.labels, err = stringMapChanges(beforeObj.GetLabels(), afterObj.GetLabels(), "label", cannotForward); err != nil {
		return nil, err
	}
	if changes.annotations, err = stringMapChanges(beforeObj.GetAnnotations(), afterObj.GetAnnotations(), "annotation", cannotForward); err != nil {
		return nil, err
	}
	beforeFinalizers := sets.New(beforeObj.GetFinalizers()...)
	afterFinalizers := sets.New(afterObj.GetFinalizers()...)
	if removed := sets.List(beforeFinalizers.Difference(afterFinalizers)); len(removed) > 0 {
		return nil, cannotForward(fmt.Sprintf("removed finalizers %q", removed))
	}
	for _, f := range afterObj.GetFinalizers() {
		if !beforeFinalizers.Has(f) {
			changes.finalizers = append(changes.finalizers, f)
		}
	}

	rest := func(obj *unstructured.Unstructured) map[string]interface{} {
		c := obj.DeepCopy()
		unstructured.RemoveNestedField(c.Object, "metadata", "labels")
		unstructured.RemoveNestedField(c.Object, "metadata", "annotations")
		unstructured.RemoveNestedField(c.Object, "metadata", "finalizers")
		if metadata, ok := c.Object["metadata"].(map[string]interface{}); ok && len(metadata) == 0 {
			delete(c.Object, "metadata")
		}
		return c.Object
	}
	if path := firstDifference(rest(beforeObj), rest(afterObj), ""); path != "" {
		return nil, cannotForward("changed " + path)
	}
	return changes, nil
}

// stringMapChanges returns the keys added or changed from before to after, or
// an error if a key was removed.
func stringMapChanges(before, after map[string]string, what string, cannotForward func(string) error) (map[string]string, error) {
	var changes map[string]string
	for key := range before {
		if _, found := after[key]; !found {
			return nil, cannotForward(fmt.Sprintf("removed %s %q", what, key))
		}
	}
	for key, value := range after {
		if old, found := before[key]; found && old == value {
			continue
		}
		if changes == nil {
			changes = map[string]string{}
		}
		changes[key] = value
	}
	return changes, nil
}

// firstDifference returns the path of the first difference between a and b, or
// "" if they are equal.
func firstDifference(a, b interface{}, path string) string {
	am, aIsMap := a.(map[string]interface{})
	bm, bIsMap := b.(map[string]interface{})
	if aIsMap && bIsMap {
		keys := sets.New[string]()
		for k := range am {
			keys.Insert(k)
		}
		for k := range bm {
			keys.Insert(k)
		}
		for _, k := range sets.List(keys) {
			av, aFound := am[k]
			bv, bFound := bm[k]
			if aFound != bFound {
				return path + "." + k
			}
			if p := firstDifference(av, bv, path+"."+k); p != "" {
				return p
			}
		}
		return ""
	}
	if equality.Semantic.DeepEqual(a, b) {
		return ""
	}
	if path == "" {
		return "."
	}
	return path
}
