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

package workspace

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kcp-dev/logicalcluster/v3"
	corev1alpha1 "github.com/kcp-dev/sdk/apis/core/v1alpha1"
	kcpclientset "github.com/kcp-dev/sdk/client/clientset/versioned/cluster"
	kcpfakeclient "github.com/kcp-dev/sdk/client/clientset/versioned/cluster/fake"
)

// A scheduling retry must not move an initialized LogicalCluster back to
// Initializing (admission would re-arm its initializers).
func TestUpdateLogicalClusterPhaseOnlyFromScheduling(t *testing.T) {
	for _, tc := range []struct {
		from, want corev1alpha1.LogicalClusterPhaseType
	}{
		{"", corev1alpha1.LogicalClusterPhaseInitializing},
		{corev1alpha1.LogicalClusterPhaseScheduling, corev1alpha1.LogicalClusterPhaseInitializing},
		{corev1alpha1.LogicalClusterPhaseInitializing, corev1alpha1.LogicalClusterPhaseInitializing},
		{corev1alpha1.LogicalClusterPhaseReady, corev1alpha1.LogicalClusterPhaseReady},
	} {
		lc := &corev1alpha1.LogicalCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:        corev1alpha1.LogicalClusterName,
				Annotations: map[string]string{logicalcluster.AnnotationKey: "c1"},
			},
			Status: corev1alpha1.LogicalClusterStatus{Phase: tc.from},
		}
		client := kcpfakeclient.NewSimpleClientset(lc) //nolint:staticcheck
		r := schedulingReconciler{kcpLogicalClusterAdminClientFor: func(*corev1alpha1.Shard) (kcpclientset.ClusterInterface, error) {
			return client, nil
		}}
		if err := r.updateLogicalClusterPhase(context.Background(), &corev1alpha1.Shard{}, logicalcluster.NewPath("c1"), corev1alpha1.LogicalClusterPhaseInitializing); err != nil {
			t.Fatalf("from %q: %v", tc.from, err)
		}
		got, err := client.Cluster(logicalcluster.NewPath("c1")).CoreV1alpha1().LogicalClusters().Get(context.Background(), corev1alpha1.LogicalClusterName, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if got.Status.Phase != tc.want {
			t.Errorf("from %q: phase %q, want %q", tc.from, got.Status.Phase, tc.want)
		}
	}
}
