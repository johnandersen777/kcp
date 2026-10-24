/*
Copyright 2026 The KCP Authors.

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

package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsinstall "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/install"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	kstorage "k8s.io/apiserver/pkg/storage"
	"sigs.k8s.io/yaml"

	"github.com/kcp-dev/logicalcluster/v3"
)

// Benchmarks use the real storage codecs (typed Lease at v1, CRDs stored as
// v1 and served as the internal type) so they measure what the kcp shard pays.

var benchScheme = func() *runtime.Scheme {
	s := runtime.NewScheme()
	apiextensionsinstall.Install(s)
	_ = coordinationv1.AddToScheme(s)
	metav1.AddToGroupVersion(s, schema.GroupVersion{Version: "v1"})
	return s
}()

func benchCodec(encode, decode runtime.GroupVersioner) runtime.Codec {
	codecs := serializer.NewCodecFactory(benchScheme)
	info, _ := runtime.SerializerInfoForMediaType(codecs.SupportedMediaTypes(), runtime.ContentTypeJSON)
	return codecs.CodecForVersions(info.Serializer, codecs.UniversalDeserializer(), encode, decode)
}

func benchCtx() context.Context {
	return genericapirequest.WithCluster(context.Background(), genericapirequest.Cluster{Name: logicalcluster.Name("root")})
}

func benchLease(name string) *coordinationv1.Lease {
	holder := "kcp-shard-" + name
	secs := int32(15)
	now := metav1.NewMicroTime(time.Now())
	return &coordinationv1.Lease{
		TypeMeta:   metav1.TypeMeta{APIVersion: "coordination.k8s.io/v1", Kind: "Lease"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-system", Labels: map[string]string{"app": "kcp"}},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: &secs, AcquireTime: &now, RenewTime: &now},
	}
}

func leaseStore(b *testing.B) kstorage.Interface {
	gv := coordinationv1.SchemeGroupVersion
	p, err := Prefixed(New(), "/registry", "/leases", func() runtime.Object { return &coordinationv1.Lease{} }, benchCodec(gv, gv))
	if err != nil {
		b.Fatal(err)
	}
	return p
}

// benchCRDs loads the kcp system CRDs and repeats them under new names until
// there are n of them.
func benchCRDs(b *testing.B, n int) []*apiextensions.CustomResourceDefinition {
	b.Helper()
	files, err := filepath.Glob("../../../config/crds/*.yaml")
	if err != nil || len(files) == 0 {
		b.Fatalf("no CRDs found: %v", err)
	}
	var base []*apiextensions.CustomResourceDefinition
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			b.Fatal(err)
		}
		var v1 apiextensionsv1.CustomResourceDefinition
		if err := yaml.Unmarshal(raw, &v1); err != nil || v1.Kind != "CustomResourceDefinition" {
			continue
		}
		var internal apiextensions.CustomResourceDefinition
		if err := benchScheme.Convert(&v1, &internal, nil); err != nil {
			b.Fatal(err)
		}
		base = append(base, &internal)
	}
	out := make([]*apiextensions.CustomResourceDefinition, 0, n)
	for i := 0; len(out) < n; i++ {
		crd := base[i%len(base)].DeepCopy()
		crd.Name = fmt.Sprintf("%s-%d", crd.Name, i)
		out = append(out, crd)
	}
	return out
}

const crdPrefix = "/apiextensions.k8s.io/customresourcedefinitions"

func crdStore(b *testing.B, s *Store, crds []*apiextensions.CustomResourceDefinition) kstorage.Interface {
	b.Helper()
	p, err := Prefixed(s, "/registry", crdPrefix,
		func() runtime.Object { return &apiextensions.CustomResourceDefinition{} },
		benchCodec(apiextensionsv1.SchemeGroupVersion, runtime.InternalGroupVersioner))
	if err != nil {
		b.Fatal(err)
	}
	for _, crd := range crds {
		if err := p.Create(benchCtx(), crdPrefix+"/"+crd.Name, crd, nil, 0); err != nil {
			b.Fatal(err)
		}
	}
	return p
}

func BenchmarkGetLease(b *testing.B) {
	p := leaseStore(b)
	if err := p.Create(benchCtx(), "/leases/kube-system/l", benchLease("l"), nil, 0); err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		var out coordinationv1.Lease
		if err := p.Get(benchCtx(), "/leases/kube-system/l", kstorage.GetOptions{}, &out); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkUpdateLease(b *testing.B) {
	p := leaseStore(b)
	if err := p.Create(benchCtx(), "/leases/kube-system/l", benchLease("l"), nil, 0); err != nil {
		b.Fatal(err)
	}
	i := 0
	for b.Loop() {
		i++
		var out coordinationv1.Lease
		err := p.GuaranteedUpdate(benchCtx(), "/leases/kube-system/l", &out, false, nil, func(in runtime.Object, _ kstorage.ResponseMeta) (runtime.Object, *uint64, error) {
			l := in.(*coordinationv1.Lease).DeepCopy()
			holder := fmt.Sprintf("holder-%d", i)
			l.Spec.HolderIdentity = &holder
			return l, nil, nil
		}, nil)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNoopUpdateLease(b *testing.B) {
	p := leaseStore(b)
	if err := p.Create(benchCtx(), "/leases/kube-system/l", benchLease("l"), nil, 0); err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		var out coordinationv1.Lease
		err := p.GuaranteedUpdate(benchCtx(), "/leases/kube-system/l", &out, false, nil, func(in runtime.Object, _ kstorage.ResponseMeta) (runtime.Object, *uint64, error) {
			return in.DeepCopyObject(), nil, nil
		}, nil)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkList69CRDs(b *testing.B) {
	p := crdStore(b, New(), benchCRDs(b, 69))
	for b.Loop() {
		var list apiextensions.CustomResourceDefinitionList
		if err := p.GetList(benchCtx(), crdPrefix, kstorage.ListOptions{Recursive: true}, &list); err != nil {
			b.Fatal(err)
		}
		if len(list.Items) != 69 {
			b.Fatalf("listed %d CRDs", len(list.Items))
		}
	}
}

// BenchmarkSnapshotTick is one coalesced persistence tick after a single
// write, over roughly 6 MB of state.
func BenchmarkSnapshotTick(b *testing.B) {
	name := "bench-snapshot-" + strings.ReplaceAll(b.Name(), "/", "-")
	crds := benchCRDs(b, 300)
	p := crdStore(b, Shared(name), crds)
	full, err := SnapshotAll()
	if err != nil {
		b.Fatal(err)
	}
	b.ReportMetric(float64(len(full))/1e6, "MB-state")
	i := 0
	for b.Loop() {
		i++
		err := p.GuaranteedUpdate(benchCtx(), crdPrefix+"/"+crds[0].Name, &apiextensions.CustomResourceDefinition{}, false, nil, func(in runtime.Object, _ kstorage.ResponseMeta) (runtime.Object, *uint64, error) {
			crd := in.(*apiextensions.CustomResourceDefinition).DeepCopy()
			crd.Labels = map[string]string{"tick": fmt.Sprint(i)}
			return crd, nil, nil
		}, nil)
		if err != nil {
			b.Fatal(err)
		}
		if _, _, err := persistMessage(false); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSnapshotFull is a full snapshot (the first tick after boot, or a
// compaction) over roughly 6 MB of state, after one write.
func BenchmarkSnapshotFull(b *testing.B) {
	crds := benchCRDs(b, 300)
	p := crdStore(b, Shared("bench-snapshot-full"), crds)
	if _, err := SnapshotAll(); err != nil {
		b.Fatal(err)
	}
	i := 0
	for b.Loop() {
		i++
		err := p.GuaranteedUpdate(benchCtx(), crdPrefix+"/"+crds[0].Name, &apiextensions.CustomResourceDefinition{}, false, nil, func(in runtime.Object, _ kstorage.ResponseMeta) (runtime.Object, *uint64, error) {
			crd := in.(*apiextensions.CustomResourceDefinition).DeepCopy()
			crd.Labels = map[string]string{"tick": fmt.Sprint(i)}
			return crd, nil, nil
		}, nil)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := SnapshotAll(); err != nil {
			b.Fatal(err)
		}
	}
}
