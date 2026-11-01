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

package options

import (
	"testing"

	cliflag "k8s.io/component-base/cli/flag"
)

func TestAPIExtensionsMasterCount(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		replicas int
		single   bool
		want     int
	}{
		{name: "default keeps the HA create sleep", replicas: 1, want: 0},
		{name: "single apiserver skips it", replicas: 1, single: true, want: 1},
		{name: "replicas keep it and delay establishing", replicas: 3, want: 3},
		{name: "replicas win over single", replicas: 2, single: true, want: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o := ExtraOptions{ShardReplicaCount: tc.replicas, SingleAPIServer: tc.single}
			if got := o.APIExtensionsMasterCount(); got != tc.want {
				t.Errorf("APIExtensionsMasterCount() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestSingleAPIServerFlag(t *testing.T) {
	t.Parallel()
	o := NewOptions(".kcp")
	if o.Extra.SingleAPIServer {
		t.Fatal("SingleAPIServer defaults to true, want false")
	}
	if got := o.Extra.APIExtensionsMasterCount(); got != 0 {
		t.Fatalf("default APIExtensionsMasterCount() = %d, want 0 (create sleep kept)", got)
	}
	var fss cliflag.NamedFlagSets
	o.AddFlags(&fss)
	if err := fss.FlagSet("kcp").Parse([]string{"--single-apiserver"}); err != nil {
		t.Fatal(err)
	}
	if got := o.Extra.APIExtensionsMasterCount(); got != 1 {
		t.Fatalf("APIExtensionsMasterCount() with --single-apiserver = %d, want 1", got)
	}
}
