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

package forwardingregistry_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/structured-merge-diff/v6/fieldpath"
	"sigs.k8s.io/yaml"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	"k8s.io/apiextensions-apiserver/pkg/registry/customresource"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/audit"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/authorization/authorizerfactory"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/apiserver/pkg/util/compatibility"
	"k8s.io/client-go/util/csaupgrade"

	kcpdynamic "github.com/kcp-dev/client-go/dynamic"
	kcpfakedynamic "github.com/kcp-dev/client-go/third_party/k8s.io/client-go/dynamic/fake"
	kcptesting "github.com/kcp-dev/client-go/third_party/k8s.io/client-go/testing"
	apisv1alpha1 "github.com/kcp-dev/sdk/apis/apis/v1alpha1"
	"github.com/kcp-dev/virtual-workspace-framework/pkg/dynamic/apidefinition"
	"github.com/kcp-dev/virtual-workspace-framework/pkg/dynamic/apiserver"
	"github.com/kcp-dev/virtual-workspace-framework/pkg/forwardingregistry"
)

// injectedLabel is set by the test admission plugin, like the APIExport
// virtual workspace's permission claim admission injects selector labels.
const injectedLabel = "claims.example.com/selected"

var noxuKind = schema.GroupVersionKind{Group: "mygroup.example.com", Version: "v1beta1", Kind: "Noxu"}

// ssaBackend is a fake backend apiserver for noxus with real server-side apply
// bookkeeping: managedFields are computed by the same field managers a kube
// apiserver uses for a CRD with a status subresource.
type ssaBackend struct {
	mu            sync.Mutex
	objects       map[string]*unstructured.Unstructured
	rv            int
	fieldManagers map[string]*managedfields.FieldManager
	calls         []string
}

func newSSABackend(t *testing.T) *ssaBackend {
	t.Helper()
	gv := fieldpath.APIVersion(noxuKind.GroupVersion().String())
	b := &ssaBackend{objects: map[string]*unstructured.Unstructured{}, fieldManagers: map[string]*managedfields.FieldManager{}}
	for subresource, reset := range map[string]string{"": "status", "status": "spec"} {
		fm, err := managedfields.NewDefaultCRDFieldManager(
			managedfields.NewDeducedTypeConverter(),
			nopConvertor{}, nopDefaulter{}, unstructuredCreater{},
			noxuKind, noxuKind.GroupVersion(), subresource,
			map[fieldpath.APIVersion]fieldpath.Filter{gv: fieldpath.NewExcludeSetFilter(fieldpath.NewSet(fieldpath.MakePathOrDie(reset)))},
		)
		require.NoError(t, err)
		b.fieldManagers[subresource] = fm
	}
	return b
}

func (b *ssaBackend) key(namespace, name string) string { return namespace + "/" + name }

func (b *ssaBackend) get(namespace, name string) *unstructured.Unstructured {
	b.mu.Lock()
	defer b.mu.Unlock()
	if obj, ok := b.objects[b.key(namespace, name)]; ok {
		return obj.DeepCopy()
	}
	return nil
}

// set stores an object as is (with a new resourceVersion), bypassing the field managers.
func (b *ssaBackend) set(obj *unstructured.Unstructured) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rv++
	obj = obj.DeepCopy()
	obj.SetResourceVersion(strconv.Itoa(b.rv))
	b.objects[b.key(obj.GetNamespace(), obj.GetName())] = obj
}

func (b *ssaBackend) callsSince(i int) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string{}, b.calls[i:]...)
}

func (b *ssaBackend) numCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.calls)
}

// prepare applies the CRD strategies: the main resource keeps the live status,
// the status subresource keeps everything but the status (and managedFields).
func prepare(live, obj *unstructured.Unstructured, subresource string) *unstructured.Unstructured {
	if subresource == "status" {
		res := live.DeepCopy()
		res.SetManagedFields(obj.GetManagedFields())
		if status, ok := obj.Object["status"]; ok {
			res.Object["status"] = status
		} else {
			delete(res.Object, "status")
		}
		return res
	}
	res := obj.DeepCopy()
	if status, ok := live.Object["status"]; ok {
		res.Object["status"] = status
	} else {
		delete(res.Object, "status")
	}
	res.SetUID(live.GetUID())
	res.SetCreationTimestamp(live.GetCreationTimestamp())
	return res
}

func (b *ssaBackend) react(action kcptesting.Action) (bool, runtime.Object, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	gr := action.GetResource().GroupResource()
	namespace := action.GetNamespace()
	subresource := action.GetSubresource()

	store := func(obj *unstructured.Unstructured, dryRun []string) *unstructured.Unstructured {
		obj = obj.DeepCopy()
		if len(dryRun) > 0 {
			return obj
		}
		b.rv++
		obj.SetResourceVersion(strconv.Itoa(b.rv))
		b.objects[b.key(namespace, obj.GetName())] = obj
		return obj.DeepCopy()
	}

	switch a := action.(type) {
	case kcptesting.GetActionImpl:
		b.calls = append(b.calls, "get")
		obj, ok := b.objects[b.key(namespace, a.Name)]
		if !ok {
			return true, nil, apierrors.NewNotFound(gr, a.Name)
		}
		return true, obj.DeepCopy(), nil

	case kcptesting.CreateActionImpl:
		b.calls = append(b.calls, "create")
		obj := a.Object.(*unstructured.Unstructured).DeepCopy()
		if _, ok := b.objects[b.key(namespace, obj.GetName())]; ok {
			return true, nil, apierrors.NewAlreadyExists(gr, obj.GetName())
		}
		live := &unstructured.Unstructured{}
		live.SetGroupVersionKind(obj.GroupVersionKind())
		out, err := b.fieldManagers[""].Update(live, obj, a.CreateOptions.FieldManager)
		if err != nil {
			return true, nil, err
		}
		created := prepare(live, out.(*unstructured.Unstructured), "")
		created.SetUID(types.UID("uid-" + obj.GetName()))
		return true, store(created, a.CreateOptions.DryRun), nil

	case kcptesting.UpdateActionImpl:
		b.calls = append(b.calls, "update/"+subresource)
		obj := a.Object.(*unstructured.Unstructured).DeepCopy()
		live, ok := b.objects[b.key(namespace, obj.GetName())]
		if !ok {
			return true, nil, apierrors.NewNotFound(gr, obj.GetName())
		}
		if obj.GetResourceVersion() != live.GetResourceVersion() {
			return true, nil, apierrors.NewConflict(gr, obj.GetName(), fmt.Errorf("the object has been modified"))
		}
		out, err := b.fieldManagers[subresource].Update(live.DeepCopy(), obj, a.UpdateOptions.FieldManager)
		if err != nil {
			return true, nil, err
		}
		return true, store(prepare(live, out.(*unstructured.Unstructured), subresource), a.UpdateOptions.DryRun), nil

	case kcptesting.PatchActionImpl:
		b.calls = append(b.calls, "patch/"+subresource+"/"+string(a.PatchType))
		if a.PatchType != types.ApplyPatchType {
			return true, nil, fmt.Errorf("fake backend only supports apply patches, got %s", a.PatchType)
		}
		patchJSON, err := yaml.YAMLToJSON(a.Patch)
		if err != nil {
			return true, nil, apierrors.NewBadRequest(err.Error())
		}
		patchObj := &unstructured.Unstructured{}
		if err := patchObj.UnmarshalJSON(patchJSON); err != nil {
			return true, nil, apierrors.NewBadRequest(err.Error())
		}
		live, exists := b.objects[b.key(namespace, a.Name)]
		if !exists {
			if subresource != "" {
				return true, nil, apierrors.NewNotFound(gr, a.Name)
			}
			live = &unstructured.Unstructured{}
			live.SetGroupVersionKind(patchObj.GroupVersionKind())
			live.SetUID(types.UID("uid-" + a.Name))
		}
		if rv := patchObj.GetResourceVersion(); rv != "" && (!exists || rv != live.GetResourceVersion()) {
			return true, nil, apierrors.NewConflict(gr, a.Name, fmt.Errorf("the object has been modified"))
		}
		force := a.PatchOptions.Force != nil && *a.PatchOptions.Force
		out, err := b.fieldManagers[subresource].Apply(live.DeepCopy(), patchObj, a.PatchOptions.FieldManager, force)
		if err != nil {
			return true, nil, err
		}
		applied := prepare(live, out.(*unstructured.Unstructured), subresource)
		applied.SetNamespace(namespace)
		applied.SetName(a.Name)
		return true, store(applied, a.PatchOptions.DryRun), nil
	}
	return false, nil, nil
}

type nopConvertor struct{}

func (nopConvertor) Convert(in, out, context interface{}) error { return nil }
func (nopConvertor) ConvertToVersion(in runtime.Object, _ runtime.GroupVersioner) (runtime.Object, error) {
	return in, nil
}
func (nopConvertor) ConvertFieldLabel(_ schema.GroupVersionKind, label, value string) (string, string, error) {
	return label, value, nil
}

type nopDefaulter struct{}

func (nopDefaulter) Default(runtime.Object) {}

type unstructuredCreater struct{}

func (unstructuredCreater) New(kind schema.GroupVersionKind) (runtime.Object, error) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(kind)
	return obj, nil
}

// labelAdmission injects a label on create and update, and validates it is
// there, like the APIExport virtual workspace's permission claim admission.
type labelAdmission struct {
	mu        sync.Mutex
	admitted  int
	validated int
}

var _ admission.MutationInterface = &labelAdmission{}
var _ admission.ValidationInterface = &labelAdmission{}

func (a *labelAdmission) Handles(operation admission.Operation) bool {
	return operation == admission.Create || operation == admission.Update
}

func (a *labelAdmission) Admit(_ context.Context, attrs admission.Attributes, _ admission.ObjectInterfaces) error {
	a.mu.Lock()
	a.admitted++
	a.mu.Unlock()
	if attrs.GetSubresource() != "" {
		return nil
	}
	u, ok := attrs.GetObject().(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("unexpected type %T", attrs.GetObject())
	}
	lbls := u.GetLabels()
	if lbls == nil {
		lbls = map[string]string{}
	}
	if _, ok := lbls[injectedLabel]; !ok {
		lbls[injectedLabel] = "true"
	}
	u.SetLabels(lbls)
	return nil
}

func (a *labelAdmission) Validate(_ context.Context, attrs admission.Attributes, _ admission.ObjectInterfaces) error {
	a.mu.Lock()
	a.validated++
	a.mu.Unlock()
	u, ok := attrs.GetObject().(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("unexpected type %T", attrs.GetObject())
	}
	if u.GetLabels()[injectedLabel] != "true" {
		return admission.NewForbidden(attrs, fmt.Errorf("label %s must be true", injectedLabel))
	}
	return nil
}

func (a *labelAdmission) counts() (int, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.admitted, a.validated
}

// vwHarness serves noxus the way the APIExport virtual workspace does:
// CreateServingInfoFor request scopes (with their field managers), the
// forwarding storage, and the virtual workspace's PATCH handler.
type vwHarness struct {
	t         *testing.T
	backend   *ssaBackend
	client    *kcpfakedynamic.FakeDynamicClusterClientset
	apiDef    apidefinition.APIDefinition
	admission *labelAdmission
}

func newVWHarness(t *testing.T, wrapper forwardingregistry.StorageWrapper) *vwHarness {
	t.Helper()

	backend := newSSABackend(t)
	client := kcpfakedynamic.NewSimpleDynamicClient(runtime.NewScheme())
	client.PrependReactor("*", "noxus", backend.react)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	restProvider := func(resource schema.GroupVersionResource, kind schema.GroupVersionKind, listKind schema.GroupVersionKind, typer runtime.ObjectTyper, tableConvertor rest.TableConvertor, namespaceScoped bool, schemaValidator validation.SchemaValidator, subresourcesSchemaValidator map[string]validation.SchemaValidator, structuralSchema *structuralschema.Structural) (rest.Storage, map[string]rest.Storage) {
		strategy := customresource.NewStrategy(
			typer, namespaceScoped, kind,
			forwardingregistry.ValidatePathSegmentName,
			schemaValidator, subresourcesSchemaValidator["status"], structuralSchema,
			&apiextensions.CustomResourceSubresourceStatus{}, nil,
			[]apiextensionsv1.SelectableField{},
		)
		storage, statusStorage := forwardingregistry.NewStorage(
			ctx, resource, "", kind, listKind, strategy, nil, tableConvertor, nil,
			func(ctx context.Context) (kcpdynamic.ClusterInterface, error) { return client, nil },
			nil, wrapper,
		)
		return storage, map[string]rest.Storage{"status": statusStorage}
	}

	codecs := serializer.NewCodecFactory(runtime.NewScheme())
	config := genericapiserver.NewConfig(codecs)
	config.ExternalAddress = "127.0.0.1:443"
	config.EffectiveVersion = compatibility.DefaultBuildEffectiveVersion()
	config.Authorization.Authorizer = authorizerfactory.NewAlwaysAllowAuthorizer()
	completed := config.Complete(nil)

	apiDef, err := apiserver.CreateServingInfoFor(completed, noxuAPIResourceSchema(), "v1beta1", restProvider)
	require.NoError(t, err)
	t.Cleanup(apiDef.TearDown)

	return &vwHarness{t: t, backend: backend, client: client, apiDef: apiDef, admission: &labelAdmission{}}
}

func noxuAPIResourceSchema() *apisv1alpha1.APIResourceSchema {
	return &apisv1alpha1.APIResourceSchema{
		ObjectMeta: metav1.ObjectMeta{Name: "v1.noxus.mygroup.example.com"},
		Spec: apisv1alpha1.APIResourceSchemaSpec{
			Group: "mygroup.example.com",
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: "noxus", Singular: "noxu", Kind: "Noxu", ListKind: "NoxuList",
			},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apisv1alpha1.APIResourceVersion{{
				Name: "v1beta1", Served: true, Storage: true,
				Schema: runtime.RawExtension{Raw: []byte(`{"type":"object","properties":{` +
					`"spec":{"type":"object","x-kubernetes-preserve-unknown-fields":true},` +
					`"status":{"type":"object","x-kubernetes-preserve-unknown-fields":true}}}`)},
				Subresources: apiextensionsv1.CustomResourceSubresources{Status: &apiextensionsv1.CustomResourceSubresourceStatus{}},
			}},
		},
	}
}

type patchResult struct {
	code int
	obj  *unstructured.Unstructured
	body string
}

// patch sends a PATCH for noxus/foo in namespace default through the virtual workspace PATCH handler.
func (h *vwHarness) patch(subresource string, patchType types.PatchType, body, query string) patchResult {
	h.t.Helper()

	storage := h.apiDef.GetStorage()
	scope := h.apiDef.GetRequestScope()
	path := "/apis/mygroup.example.com/v1beta1/namespaces/default/noxus/foo"
	if subresource != "" {
		storage = h.apiDef.GetSubResourceStorage(subresource)
		scope = h.apiDef.GetSubResourceRequestScope(subresource)
		path += "/" + subresource
	}
	if query != "" {
		path += "?" + query
	}

	req := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(body))
	req.Header.Set("Content-Type", string(patchType))
	req.Header.Set("Accept", "application/json")
	ctx := request.WithRequestInfo(req.Context(), &request.RequestInfo{
		IsResourceRequest: true,
		Path:              req.URL.Path,
		Verb:              "patch",
		APIGroup:          "mygroup.example.com",
		APIVersion:        "v1beta1",
		Namespace:         "default",
		Resource:          "noxus",
		Subresource:       subresource,
		Name:              "foo",
	})
	ctx = request.WithNamespace(ctx, "default")
	ctx = request.WithCluster(ctx, request.Cluster{Name: "test"})
	ctx = request.WithUser(ctx, &user.DefaultInfo{Name: "provider"})
	ctx = audit.WithAuditContext(ctx)
	req = req.WithContext(ctx)

	supportedTypes := []string{string(types.JSONPatchType), string(types.MergePatchType), string(types.ApplyPatchType)}
	handler := apiserver.PatchResource(storage.(rest.Patcher), scope, h.admission, supportedTypes)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	res := patchResult{code: w.Code, body: w.Body.String()}
	if w.Code < 300 {
		res.obj = &unstructured.Unstructured{}
		require.NoError(h.t, res.obj.UnmarshalJSON(w.Body.Bytes()), w.Body.String())
	}
	return res
}

func (h *vwHarness) apply(subresource, manager, body string, extraQuery ...string) patchResult {
	h.t.Helper()
	query := "fieldManager=" + manager
	for _, q := range extraQuery {
		query += "&" + q
	}
	return h.patch(subresource, types.ApplyPatchType, body, query)
}

func managedFieldsOf(obj *unstructured.Unstructured, manager, subresource string) []metav1.ManagedFieldsEntry {
	var entries []metav1.ManagedFieldsEntry
	for _, e := range obj.GetManagedFields() {
		if e.Manager == manager && e.Subresource == subresource {
			entries = append(entries, e)
		}
	}
	return entries
}

func describeManagedFields(obj *unstructured.Unstructured) string {
	var lines []string
	for _, e := range obj.GetManagedFields() {
		raw := ""
		if e.FieldsV1 != nil {
			raw = string(e.FieldsV1.Raw)
		}
		lines = append(lines, fmt.Sprintf("%s op=%s sub=%q %s", e.Manager, e.Operation, e.Subresource, raw))
	}
	return strings.Join(lines, "\n")
}

func requireOnlyApply(t *testing.T, obj *unstructured.Unstructured, manager, subresource string) {
	t.Helper()
	entries := managedFieldsOf(obj, manager, subresource)
	require.Len(t, entries, 1, "expected one managedFields entry for %s (subresource %q):\n%s", manager, subresource, describeManagedFields(obj))
	require.Equal(t, metav1.ManagedFieldsOperationApply, entries[0].Operation, describeManagedFields(obj))
}

const specApplyAB = `apiVersion: mygroup.example.com/v1beta1
kind: Noxu
metadata:
  name: foo
  namespace: default
spec:
  a: "1"
  b: "2"
`

const specApplyA = `apiVersion: mygroup.example.com/v1beta1
kind: Noxu
metadata:
  name: foo
  namespace: default
spec:
  a: "1"
`

const statusApplyWithPending = `apiVersion: mygroup.example.com/v1beta1
kind: Noxu
metadata:
  name: foo
  namespace: default
status:
  phase: Ready
  pendingAction: SignIn
`

const statusApplyWithoutPending = `apiVersion: mygroup.example.com/v1beta1
kind: Noxu
metadata:
  name: foo
  namespace: default
status:
  phase: Ready
`

// TestApplyThroughVirtualWorkspace shows that a server-side apply sent through
// the virtual workspace reaches the backend as an apply: the backend records
// the field manager with operation Apply, and a later apply by the same
// manager that omits a field removes it (for spec and for status).
func TestApplyThroughVirtualWorkspace(t *testing.T) {
	t.Parallel()
	h := newVWHarness(t, nil)

	// Create through apply.
	before := h.backend.numCalls()
	res := h.apply("", "provider", specApplyAB)
	require.Equal(t, http.StatusOK, res.code, res.body)
	require.Contains(t, h.backend.callsSince(before), "patch//"+string(types.ApplyPatchType), "the apply must be forwarded as an apply patch")
	require.NotContains(t, h.backend.callsSince(before), "create")
	live := h.backend.get("default", "foo")
	require.NotNil(t, live)
	requireOnlyApply(t, live, "provider", "")
	require.Equal(t, "true", live.GetLabels()[injectedLabel], "the label injected by mutating admission must be stored")

	// Apply again, omitting spec.b: it must be removed.
	before = h.backend.numCalls()
	res = h.apply("", "provider", specApplyA)
	require.Equal(t, http.StatusOK, res.code, res.body)
	require.NotContains(t, h.backend.callsSince(before), "update/")
	live = h.backend.get("default", "foo")
	_, found, _ := unstructured.NestedString(live.Object, "spec", "b")
	require.False(t, found, "spec.b must be removed by an apply that omits it; managedFields:\n%s", describeManagedFields(live))
	a, _, _ := unstructured.NestedString(live.Object, "spec", "a")
	require.Equal(t, "1", a)
	requireOnlyApply(t, live, "provider", "")
	require.Equal(t, "true", live.GetLabels()[injectedLabel], "the label injected by mutating admission must survive a later apply")
	admitted, validated := h.admission.counts()
	require.Positive(t, admitted, "mutating admission must run")
	require.Positive(t, validated, "validating admission must run")

	// Status: set pendingAction, then omit it.
	res = h.apply("status", "provider-status", statusApplyWithPending)
	require.Equal(t, http.StatusOK, res.code, res.body)
	live = h.backend.get("default", "foo")
	pending, _, _ := unstructured.NestedString(live.Object, "status", "pendingAction")
	require.Equal(t, "SignIn", pending)
	requireOnlyApply(t, live, "provider-status", "status")

	before = h.backend.numCalls()
	res = h.apply("status", "provider-status", statusApplyWithoutPending)
	require.Equal(t, http.StatusOK, res.code, res.body)
	require.Contains(t, h.backend.callsSince(before), "patch/status/"+string(types.ApplyPatchType))
	live = h.backend.get("default", "foo")
	_, found, _ = unstructured.NestedString(live.Object, "status", "pendingAction")
	require.False(t, found, "status.pendingAction must be removed by an apply that omits it; managedFields:\n%s", describeManagedFields(live))
	phase, _, _ := unstructured.NestedString(live.Object, "status", "phase")
	require.Equal(t, "Ready", phase)
	requireOnlyApply(t, live, "provider-status", "status")
	_, found, _ = unstructured.NestedString(live.Object, "spec", "a")
	require.True(t, found, "a status apply must not touch spec")
}

func TestApplyThroughVirtualWorkspaceConflictsForceAndDryRun(t *testing.T) {
	t.Parallel()
	h := newVWHarness(t, nil)

	res := h.apply("", "provider", specApplyA)
	require.Equal(t, http.StatusOK, res.code, res.body)

	other := strings.Replace(specApplyA, `a: "1"`, `a: "other"`, 1)

	// Another manager changing an owned field conflicts...
	res = h.apply("", "other", other)
	require.Equal(t, http.StatusConflict, res.code, res.body)
	a, _, _ := unstructured.NestedString(h.backend.get("default", "foo").Object, "spec", "a")
	require.Equal(t, "1", a)

	// ... a dry run with force changes nothing...
	before := h.backend.numCalls()
	res = h.apply("", "other", other, "force=true", "dryRun=All")
	require.Equal(t, http.StatusOK, res.code, res.body)
	require.Contains(t, h.backend.callsSince(before), "patch//"+string(types.ApplyPatchType))
	dryA, _, _ := unstructured.NestedString(res.obj.Object, "spec", "a")
	require.Equal(t, "other", dryA, "the dry run response shows the forced value")
	a, _, _ = unstructured.NestedString(h.backend.get("default", "foo").Object, "spec", "a")
	require.Equal(t, "1", a, "a dry run must not persist")

	// ... and force takes the field over.
	res = h.apply("", "other", other, "force=true")
	require.Equal(t, http.StatusOK, res.code, res.body)
	live := h.backend.get("default", "foo")
	a, _, _ = unstructured.NestedString(live.Object, "spec", "a")
	require.Equal(t, "other", a)
	requireOnlyApply(t, live, "other", "")
}

// TestApplyThroughVirtualWorkspaceKeepsOtherPatchTypes shows a merge patch is
// still computed by the virtual workspace and stored with an update.
func TestApplyThroughVirtualWorkspaceKeepsOtherPatchTypes(t *testing.T) {
	t.Parallel()
	h := newVWHarness(t, nil)

	res := h.apply("", "provider", specApplyA)
	require.Equal(t, http.StatusOK, res.code, res.body)

	before := h.backend.numCalls()
	res = h.patch("", types.MergePatchType, `{"spec":{"c":"3"}}`, "fieldManager=merger")
	require.Equal(t, http.StatusOK, res.code, res.body)
	require.Contains(t, h.backend.callsSince(before), "update/")
	live := h.backend.get("default", "foo")
	c, _, _ := unstructured.NestedString(live.Object, "spec", "c")
	require.Equal(t, "3", c)
	entries := managedFieldsOf(live, "merger", "")
	require.Len(t, entries, 1, describeManagedFields(live))
	require.Equal(t, metav1.ManagedFieldsOperationUpdate, entries[0].Operation)
}

// TestApplyThroughVirtualWorkspaceStaleUpdateEntry covers objects written
// before the fix: the manager owns status.pendingAction through an Update
// entry. An apply that omits the field keeps it (that is what a kube apiserver
// does too: the Update entry still owns it). Upgrading the manager's own
// Update entry into its Apply entry (client-go csaupgrade), sent as a JSON
// patch on the main resource, fixes the object; the next apply removes the
// field. Dropping the Update entry instead would leave the field unowned, and
// an apply never removes fields it did not own.
func TestApplyThroughVirtualWorkspaceStaleUpdateEntry(t *testing.T) {
	t.Parallel()
	h := newVWHarness(t, nil)

	// The object as the old forwarding left it: status fields owned by an Update entry.
	res := h.apply("", "provider", specApplyA)
	require.Equal(t, http.StatusOK, res.code, res.body)
	live := h.backend.get("default", "foo")
	withStatus := live.DeepCopy()
	require.NoError(t, unstructured.SetNestedField(withStatus.Object, "Ready", "status", "phase"))
	require.NoError(t, unstructured.SetNestedField(withStatus.Object, "SignIn", "status", "pendingAction"))
	out, err := h.backend.fieldManagers["status"].Update(live, withStatus, "provider-status")
	require.NoError(t, err)
	h.backend.set(out.(*unstructured.Unstructured))
	live = h.backend.get("default", "foo")
	stale := managedFieldsOf(live, "provider-status", "status")
	require.Len(t, stale, 1)
	require.Equal(t, metav1.ManagedFieldsOperationUpdate, stale[0].Operation)

	// An apply omitting pendingAction keeps it: the stale Update entry still owns it.
	res = h.apply("status", "provider-status", statusApplyWithoutPending)
	require.Equal(t, http.StatusOK, res.code, res.body)
	live = h.backend.get("default", "foo")
	pending, _, _ := unstructured.NestedString(live.Object, "status", "pendingAction")
	require.Equal(t, "SignIn", pending, describeManagedFields(live))

	// One-time migration: merge the manager's own Update entry into its Apply entry.
	migration, err := csaupgrade.UpgradeManagedFieldsPatch(live, sets.New("provider-status"), "provider-status", csaupgrade.Subresource("status"))
	require.NoError(t, err)
	require.NotEmpty(t, migration)
	var ops []map[string]interface{}
	require.NoError(t, json.Unmarshal(migration, &ops))
	res = h.patch("", types.JSONPatchType, string(migration), "fieldManager=migration")
	require.Equal(t, http.StatusOK, res.code, res.body)
	live = h.backend.get("default", "foo")
	requireOnlyApply(t, live, "provider-status", "status")
	require.Empty(t, managedFieldsOf(live, "migration", ""), "the migration patch must not own fields")
	require.Len(t, managedFieldsOf(live, "provider", ""), 1, "other managers' entries must be kept:\n%s", describeManagedFields(live))

	// Now the apply removes it.
	res = h.apply("status", "provider-status", statusApplyWithoutPending)
	require.Equal(t, http.StatusOK, res.code, res.body)
	live = h.backend.get("default", "foo")
	_, found, _ := unstructured.NestedString(live.Object, "status", "pendingAction")
	require.False(t, found, describeManagedFields(live))
	phase, _, _ := unstructured.NestedString(live.Object, "status", "phase")
	require.Equal(t, "Ready", phase)
}

// TestApplyThroughVirtualWorkspaceConcurrentWrite shows the forwarded apply is
// conditional on the object the virtual workspace admitted: a write between
// the virtual workspace's Get and the apply makes the backend answer Conflict,
// and the virtual workspace retries on the fresh object.
func TestApplyThroughVirtualWorkspaceConcurrentWrite(t *testing.T) {
	t.Parallel()
	h := newVWHarness(t, nil)

	res := h.apply("", "provider", specApplyAB)
	require.Equal(t, http.StatusOK, res.code, res.body)

	var once sync.Once
	h.client.PrependReactor("patch", "noxus", func(action kcptesting.Action) (bool, runtime.Object, error) {
		once.Do(func() {
			// Another client writes spec.other in between.
			live := h.backend.get("default", "foo")
			require.NoError(t, unstructured.SetNestedField(live.Object, "x", "spec", "other"))
			h.backend.set(live)
		})
		return false, nil, nil
	})

	before := h.backend.numCalls()
	res = h.apply("", "provider", specApplyA)
	require.Equal(t, http.StatusOK, res.code, res.body)
	var patches int
	for _, call := range h.backend.callsSince(before) {
		if strings.HasPrefix(call, "patch/") {
			patches++
		}
	}
	require.Equal(t, 2, patches, "the conflicting apply must be retried once: %v", h.backend.callsSince(before))
	live := h.backend.get("default", "foo")
	other, _, _ := unstructured.NestedString(live.Object, "spec", "other")
	require.Equal(t, "x", other, "the concurrent write must be kept")
	_, found, _ := unstructured.NestedString(live.Object, "spec", "b")
	require.False(t, found)
}

// TestApplyThroughVirtualWorkspaceHiddenObject shows an apply cannot modify an
// object the storage hides (here with a label selector): as with the Create it
// replaces, it fails with AlreadyExists.
func TestApplyThroughVirtualWorkspaceHiddenObject(t *testing.T) {
	t.Parallel()
	requirement, err := labels.NewRequirement("visible", selection.Equals, []string{"true"})
	require.NoError(t, err)
	h := newVWHarness(t, forwardingregistry.WithStaticLabelSelector(labels.Requirements{*requirement}))

	hidden := &unstructured.Unstructured{}
	hidden.SetGroupVersionKind(noxuKind)
	hidden.SetNamespace("default")
	hidden.SetName("foo")
	require.NoError(t, unstructured.SetNestedField(hidden.Object, "hidden", "spec", "a"))
	h.backend.set(hidden)

	before := h.backend.numCalls()
	res := h.apply("", "provider", specApplyA)
	require.Equal(t, http.StatusConflict, res.code, res.body)
	require.Contains(t, res.body, "AlreadyExists")
	for _, call := range h.backend.callsSince(before) {
		require.False(t, strings.HasPrefix(call, "patch/"), "a hidden object must not be patched: %v", h.backend.callsSince(before))
	}
	a, _, _ := unstructured.NestedString(h.backend.get("default", "foo").Object, "spec", "a")
	require.Equal(t, "hidden", a)
}
