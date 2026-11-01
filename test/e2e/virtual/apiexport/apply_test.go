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

package apiexport

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"

	kcpdynamic "github.com/kcp-dev/client-go/dynamic"
	"github.com/kcp-dev/logicalcluster/v3"
	"github.com/kcp-dev/sdk/apis/core"
	kcpclientset "github.com/kcp-dev/sdk/client/clientset/versioned/cluster"
	kcptesting "github.com/kcp-dev/sdk/testing"
	kcptestinghelpers "github.com/kcp-dev/sdk/testing/helpers"

	"github.com/kcp-dev/kcp/test/e2e/framework"
)

// TestAPIExportVirtualWorkspaceServerSideApply checks that a server-side apply
// through the APIExport virtual workspace is an apply on the shard: the field
// manager is recorded with operation Apply, and a later apply that omits a
// field removes it, for objects created through apply and for status.
func TestAPIExportVirtualWorkspaceServerSideApply(t *testing.T) {
	t.Parallel()
	framework.Suite(t, "control-plane")

	server := kcptesting.SharedKcpServer(t)
	cfg := server.BaseConfig(t)

	kcpClients, err := kcpclientset.NewForConfig(cfg)
	require.NoError(t, err)
	dynamicClusterClient, err := kcpdynamic.NewForConfig(cfg)
	require.NoError(t, err)

	orgPath, _ := kcptesting.NewWorkspaceFixture(t, server, core.RootCluster.Path(), kcptesting.WithType(core.RootCluster.Path(), "organization"))
	serviceProviderPath, _ := kcptesting.NewWorkspaceFixture(t, server, orgPath)
	consumerPath, consumerWorkspace := kcptesting.NewWorkspaceFixture(t, server, orgPath)
	consumerClusterName := logicalcluster.Name(consumerWorkspace.Spec.Cluster)

	setUpServiceProvider(t, dynamicClusterClient, kcpClients, false, serviceProviderPath, cfg, nil)
	bindConsumerToProvider(t, consumerPath, serviceProviderPath, kcpClients, cfg)

	t.Logf("Waiting for the APIExport virtual workspace URL for the consumer workspace")
	vwCfg := rest.CopyConfig(cfg)
	kcptestinghelpers.Eventually(t, func() (bool, string) {
		slice, err := kcpClients.Cluster(serviceProviderPath).ApisV1alpha1().APIExportEndpointSlices().Get(t.Context(), "today-cowboys", metav1.GetOptions{})
		if kcptestinghelpers.TolerateOrFail(t, err, apierrors.IsNotFound) {
			return false, fmt.Sprintf("waiting on APIExportEndpointSlice: %v", err)
		}
		var found bool
		vwCfg.Host, found, err = framework.VirtualWorkspaceURL(t.Context(), kcpClients, consumerWorkspace, framework.ExportVirtualWorkspaceURLs(slice))
		require.NoError(t, err)
		return found, fmt.Sprintf("waiting for virtual workspace URLs: %v", slice.Status.APIExportEndpoints)
	}, wait.ForeverTestTimeout, 100*time.Millisecond)

	vwDynamic, err := kcpdynamic.NewForConfig(vwCfg)
	require.NoError(t, err)
	gvr := schema.GroupVersionResource{Group: "wildwest.dev", Version: "v1alpha1", Resource: "cowboys"}
	vwCowboys := vwDynamic.Cluster(consumerClusterName.Path()).Resource(gvr).Namespace("default")
	shardCowboys := dynamicClusterClient.Cluster(consumerPath).Resource(gvr).Namespace("default")

	const manager = "e2e-provider"
	apply := func(subresource, body string) (*unstructured.Unstructured, error) {
		var subresources []string
		if subresource != "" {
			subresources = []string{subresource}
		}
		return vwCowboys.Patch(t.Context(), "ssa", types.ApplyPatchType, []byte(body), metav1.PatchOptions{FieldManager: manager}, subresources...)
	}
	get := func() *unstructured.Unstructured {
		obj, err := shardCowboys.Get(t.Context(), "ssa", metav1.GetOptions{})
		require.NoError(t, err)
		return obj
	}
	requireOnlyApply := func(obj *unstructured.Unstructured, subresource string) {
		t.Helper()
		var ops []metav1.ManagedFieldsOperationType
		for _, e := range obj.GetManagedFields() {
			if e.Manager == manager && e.Subresource == subresource {
				ops = append(ops, e.Operation)
			}
		}
		require.Equal(t, []metav1.ManagedFieldsOperationType{metav1.ManagedFieldsOperationApply}, ops, "managedFields: %v", obj.GetManagedFields())
	}

	const withIntent = `{"apiVersion":"wildwest.dev/v1alpha1","kind":"Cowboy","metadata":{"name":"ssa","namespace":"default","annotations":{"a":"1","b":"2"}},"spec":{"intent":"ride"}}`
	const withoutIntent = `{"apiVersion":"wildwest.dev/v1alpha1","kind":"Cowboy","metadata":{"name":"ssa","namespace":"default","annotations":{"a":"1"}}}`

	t.Logf("Create through apply")
	kcptestinghelpers.Eventually(t, func() (bool, string) {
		_, err := apply("", withIntent)
		return err == nil, fmt.Sprintf("waiting for the apply to succeed: %v", err)
	}, wait.ForeverTestTimeout, 100*time.Millisecond)
	obj := get()
	requireOnlyApply(obj, "")
	intent, _, _ := unstructured.NestedString(obj.Object, "spec", "intent")
	require.Equal(t, "ride", intent)

	t.Logf("Apply again omitting spec.intent and an annotation: both must be removed")
	_, err = apply("", withoutIntent)
	require.NoError(t, err)
	obj = get()
	_, found, _ := unstructured.NestedString(obj.Object, "spec", "intent")
	require.False(t, found, "spec.intent must be removed; managedFields: %v", obj.GetManagedFields())
	require.Equal(t, "1", obj.GetAnnotations()["a"])
	require.NotContains(t, obj.GetAnnotations(), "b", "annotation b must be removed")
	requireOnlyApply(obj, "")

	t.Logf("Status: apply a result, then omit it")
	_, err = apply("status", `{"apiVersion":"wildwest.dev/v1alpha1","kind":"Cowboy","metadata":{"name":"ssa","namespace":"default"},"status":{"result":"won"}}`)
	require.NoError(t, err)
	obj = get()
	result, _, _ := unstructured.NestedString(obj.Object, "status", "result")
	require.Equal(t, "won", result)
	requireOnlyApply(obj, "status")

	_, err = apply("status", `{"apiVersion":"wildwest.dev/v1alpha1","kind":"Cowboy","metadata":{"name":"ssa","namespace":"default"},"status":{}}`)
	require.NoError(t, err)
	obj = get()
	_, found, _ = unstructured.NestedString(obj.Object, "status", "result")
	require.False(t, found, "status.result must be removed; managedFields: %v", obj.GetManagedFields())
	requireOnlyApply(obj, "status")

	t.Logf("An apply that creates and carries a uid is rejected, as by a kube apiserver")
	_, err = vwCowboys.Patch(t.Context(), "ssa-uid", types.ApplyPatchType,
		[]byte(`{"apiVersion":"wildwest.dev/v1alpha1","kind":"Cowboy","metadata":{"name":"ssa-uid","namespace":"default","uid":"66666666-2222-3333-4444-555555555555"}}`),
		metav1.PatchOptions{FieldManager: manager})
	require.True(t, apierrors.IsConflict(err), "expected a Conflict, got %v", err)
	_, err = shardCowboys.Get(t.Context(), "ssa-uid", metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(err), "expected nothing created, got %v", err)

	t.Logf("A merge patch is still recorded as an Update")
	_, err = vwCowboys.Patch(t.Context(), "ssa", types.MergePatchType, []byte(`{"metadata":{"annotations":{"merged":"yes"}}}`), metav1.PatchOptions{FieldManager: "e2e-merger"})
	require.NoError(t, err)
	obj = get()
	var mergerOps []metav1.ManagedFieldsOperationType
	for _, e := range obj.GetManagedFields() {
		if e.Manager == "e2e-merger" {
			mergerOps = append(mergerOps, e.Operation)
		}
	}
	require.Equal(t, []metav1.ManagedFieldsOperationType{metav1.ManagedFieldsOperationUpdate}, mergerOps)
}
