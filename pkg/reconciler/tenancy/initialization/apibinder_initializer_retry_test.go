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

package initialization

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/kcp-dev/logicalcluster/v3"
	apisv1alpha2 "github.com/kcp-dev/sdk/apis/apis/v1alpha2"
	corev1alpha1 "github.com/kcp-dev/sdk/apis/core/v1alpha1"
	tenancyv1alpha1 "github.com/kcp-dev/sdk/apis/tenancy/v1alpha1"
	"github.com/kcp-dev/sdk/apis/third_party/conditions/util/conditions"
)

type fixedResolver []*tenancyv1alpha1.WorkspaceType

func (r fixedResolver) Resolve(*tenancyv1alpha1.WorkspaceType) ([]*tenancyv1alpha1.WorkspaceType, error) {
	return r, nil
}

// A default APIBinding create that fails transiently (an admission error at
// boot) must be retried: returning nil left the workspace Initializing for
// good, since nothing else requeues the LogicalCluster.
func TestReconcileRetriesAFailedAPIBindingCreate(t *testing.T) {
	wt := &tenancyv1alpha1.WorkspaceType{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "universal",
			Annotations: map[string]string{logicalcluster.AnnotationKey: "root"},
		},
		Spec: tenancyv1alpha1.WorkspaceTypeSpec{
			DefaultAPIBindings: []tenancyv1alpha1.APIExportReference{{Path: "root", Export: "tenancy.kcp.io"}},
		},
	}
	lc := func() *corev1alpha1.LogicalCluster {
		return &corev1alpha1.LogicalCluster{ObjectMeta: metav1.ObjectMeta{
			Name: corev1alpha1.LogicalClusterName,
			Annotations: map[string]string{
				logicalcluster.AnnotationKey:                    "app-cluster",
				tenancyv1alpha1.LogicalClusterTypeAnnotationKey: "root:universal",
			},
		}}
	}
	binder := func(createErr error) *APIBinder {
		return &APIBinder{
			getWorkspaceType:       func(logicalcluster.Path, string) (*tenancyv1alpha1.WorkspaceType, error) { return wt, nil },
			transitiveTypeResolver: fixedResolver{wt},
			listAPIBindings:        func(logicalcluster.Name) ([]*apisv1alpha2.APIBinding, error) { return nil, nil },
			getAPIBinding: func(_ logicalcluster.Name, name string) (*apisv1alpha2.APIBinding, error) {
				return nil, apierrors.NewNotFound(schema.GroupResource{Group: "apis.kcp.io", Resource: "apibindings"}, name)
			},
			getAPIExport: func(_ logicalcluster.Path, name string) (*apisv1alpha2.APIExport, error) {
				return &apisv1alpha2.APIExport{ObjectMeta: metav1.ObjectMeta{Name: name}}, nil
			},
			createAPIBinding: func(context.Context, logicalcluster.Path, *apisv1alpha2.APIBinding) (*apisv1alpha2.APIBinding, error) {
				return nil, createErr
			},
		}
	}

	transient := apierrors.NewInternalError(context.DeadlineExceeded)
	cluster := lc()
	err := binder(transient).reconcile(context.Background(), cluster)
	require.Error(t, err, "a failed create must be retried (rate-limited requeue)")
	require.True(t, conditions.IsFalse(cluster, tenancyv1alpha1.WorkspaceAPIBindingsInitialized))
	require.Equal(t, tenancyv1alpha1.WorkspaceInitializedAPIBindingErrors, conditions.GetReason(cluster, tenancyv1alpha1.WorkspaceAPIBindingsInitialized))

	// AlreadyExists is not a failure: it goes on to wait for the binding.
	exists := apierrors.NewAlreadyExists(schema.GroupResource{Group: "apis.kcp.io", Resource: "apibindings"}, "b")
	require.NoError(t, binder(exists).reconcile(context.Background(), lc()))
}
