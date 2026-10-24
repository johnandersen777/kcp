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

// Package server implements the smallest kcp-compatible API surface needed
// to serve the Fetch CRD and to make it usable with kubectl: discovery,
// CRUD, status subresource, merge/apply patches and server-side Table
// conversion.
//
// The package is deliberately free of syscall/js so that it can be unit
// tested natively with `go test ./contrib/wasm/server`. The js/wasm entrypoint
// in contrib/wasm wires it to the Deno host.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kstorage "k8s.io/apiserver/pkg/storage"
	"sigs.k8s.io/yaml"

	fetchv1alpha1 "github.com/kcp-dev/kcp/contrib/wasm/apis/fetch/v1alpha1"
	memstore "github.com/kcp-dev/kcp/contrib/wasm/storage"
)

const (
	fetchPrefix    = "/apis/" + fetchv1alpha1.GroupVersion + "/" + fetchv1alpha1.Plural
	crdGroupPath   = "/apis/apiextensions.k8s.io"
	crdVersionPath = crdGroupPath + "/v1"
	crdPath        = crdVersionPath + "/customresourcedefinitions"

	// fetchStorePrefix is the key prefix used in the in-memory
	// storage.Interface backend.
	fetchStorePrefix = "/fetches/"
)

func fetchKey(name string) string { return fetchStorePrefix + name }

// Request is the wire format the Deno host sends for every HTTP request.
type Request struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    string            `json:"body,omitempty"`
}

// Response is the wire format the Deno host turns back into an HTTP response.
// Bodies that are not valid UTF-8 (e.g. the protobuf OpenAPI document) are
// transported base64-encoded.
type Response struct {
	Status     int               `json:"status"`
	Headers    map[string]string `json:"headers,omitempty"`
	Body       string            `json:"body,omitempty"`
	BodyBase64 string            `json:"bodyBase64,omitempty"`
}

// Server is the in-memory kcp shard.
type Server struct {
	store *memstore.Store

	mu        sync.Mutex
	queue     chan string
	queued    map[string]struct{}
	stopCh    chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once

	client *http.Client
}

// New returns a ready-to-use server. Call Start to run the Fetch controller.
func New() *Server {
	return &Server{
		store:  memstore.New(),
		queue:  make(chan string, 1024),
		queued: map[string]struct{}{},
		stopCh: make(chan struct{}),
		client: &http.Client{Timeout: defaultFetchTimeout},
	}
}

// Start launches the Fetch controller.
func (s *Server) Start() {
	s.startOnce.Do(func() {
		go s.controlLoop()
	})
}

// Stop terminates the controller and the snapshot goroutine.
func (s *Server) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
		s.store.Close()
	})
}

// RestoreSnapshot loads objects and the resource version from a JSON
// snapshot produced by Store.Snapshot.
func (s *Server) RestoreSnapshot(data []byte) error {
	return s.store.Restore(data, func(raw []byte) (runtime.Object, error) {
		var f fetchv1alpha1.Fetch
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, err
		}
		return &f, nil
	})
}

// SetSnapshotHandler registers a callback invoked with the latest JSON
// snapshot after every mutation.
func (s *Server) SetSnapshotHandler(fn func([]byte)) {
	s.store.SetSnapshotHandler(fn)
}

// HandleJSON is the entrypoint used by the js/wasm bridge. It accepts a
// JSON-encoded Request and returns a JSON-encoded Response.
func (s *Server) HandleJSON(in string) (out string) {
	defer func() {
		if r := recover(); r != nil {
			b, _ := json.Marshal(errorResponse(http.StatusInternalServerError, metav1.StatusReasonInternalError, fmt.Sprintf("panic: %v", r)))
			out = string(b)
		}
	}()

	var req Request
	if err := json.Unmarshal([]byte(in), &req); err != nil {
		b, _ := json.Marshal(errorResponse(http.StatusBadRequest, metav1.StatusReasonBadRequest, "invalid request JSON: "+err.Error()))
		return string(b)
	}
	if req.Path == "" {
		req.Path = "/"
	}
	resp := s.Handle(&req)
	b, err := json.Marshal(resp)
	if err != nil {
		fallback, _ := json.Marshal(errorResponse(http.StatusInternalServerError, metav1.StatusReasonInternalError, err.Error()))
		return string(fallback)
	}
	return string(b)
}

// Handle routes one request.
func (s *Server) Handle(req *Request) *Response {
	u, err := url.Parse(req.Path)
	if err != nil {
		return errorResponse(http.StatusBadRequest, metav1.StatusReasonBadRequest, "invalid path: "+err.Error())
	}
	p := u.Path
	if p == "" {
		p = "/"
	}

	switch p {
	case "/version":
		return jsonResponse(http.StatusOK, map[string]string{
			"major":      "1",
			"minor":      "37",
			"gitVersion": "v1.37.0+kcp-wasm",
			"gitCommit":  "wasm",
			"platform":   "js/wasm",
		})
	case "/livez", "/readyz", "/healthz", "/healthz/ready":
		return textResponse(http.StatusOK, "ok")
	case "/api":
		return jsonResponse(http.StatusOK, metav1.APIVersions{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "APIVersions"},
			Versions: []string{"v1"},
			ServerAddressByClientCIDRs: []metav1.ServerAddressByClientCIDR{
				{ClientCIDR: "0.0.0.0/0", ServerAddress: ""},
			},
		})
	case "/api/v1":
		// No core (v1) resources are implemented; an empty, well-formed
		// resource list keeps discovery happy without advertising endpoints
		// that would 404.
		return jsonResponse(http.StatusOK, metav1.APIResourceList{
			TypeMeta:     metav1.TypeMeta{APIVersion: "v1", Kind: "APIResourceList"},
			GroupVersion: "v1",
			APIResources: []metav1.APIResource{},
		})
	case "/apis":
		return jsonResponse(http.StatusOK, s.apiGroupList())
	case "/apis/" + fetchv1alpha1.GroupName:
		return jsonResponse(http.StatusOK, apiGroup(fetchv1alpha1.GroupName, fetchv1alpha1.Version))
	case "/apis/" + fetchv1alpha1.GroupVersion:
		return jsonResponse(http.StatusOK, fetchAPIResourceList())
	case crdGroupPath:
		return jsonResponse(http.StatusOK, apiGroup("apiextensions.k8s.io", "v1"))
	case crdVersionPath:
		return jsonResponse(http.StatusOK, crdAPIResourceList())
	}

	switch {
	case strings.HasPrefix(p, fetchPrefix):
		return s.handleFetches(req, strings.TrimPrefix(p, fetchPrefix), u.Query())
	case p == crdPath:
		if req.Method != http.MethodGet {
			return errorResponse(http.StatusMethodNotAllowed, metav1.StatusReasonMethodNotAllowed, "only GET is supported")
		}
		return s.handleCRDList(req)
	case strings.HasPrefix(p, crdPath+"/"):
		if req.Method != http.MethodGet {
			return errorResponse(http.StatusMethodNotAllowed, metav1.StatusReasonMethodNotAllowed, "only GET is supported")
		}
		return s.handleCRD(strings.TrimPrefix(p, crdPath+"/"))
	case strings.HasPrefix(p, "/openapi/"):
		// kubectl asks for OpenAPI when explaining or applying with
		// validation. kubectl always proto-decodes /openapi/v2, so serve a
		// minimal valid protobuf document for protobuf clients and a JSON
		// stub for /openapi/v3.
		if strings.Contains(strings.ToLower(header(req.Headers, "accept")), "protobuf") {
			resp, err := openAPIV2Protobuf()
			if err != nil {
				return errorResponse(http.StatusInternalServerError, metav1.StatusReasonInternalError, err.Error())
			}
			return resp
		}
		return jsonResponse(http.StatusOK, map[string]any{
			"openapi":    "3.0.0",
			"info":       map[string]string{"title": "kcp-wasm", "version": "v1.37.0"},
			"paths":      map[string]any{},
			"components": map[string]any{},
		})
	}

	return errorResponse(http.StatusNotFound, metav1.StatusReasonNotFound, "the server could not find the requested resource: "+p)
}

func (s *Server) apiGroupList() metav1.APIGroupList {
	return metav1.APIGroupList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "APIGroupList"},
		Groups: []metav1.APIGroup{
			apiGroup(fetchv1alpha1.GroupName, fetchv1alpha1.Version),
			apiGroup("apiextensions.k8s.io", "v1"),
		},
	}
}

func apiGroup(name, version string) metav1.APIGroup {
	gv := metav1.GroupVersionForDiscovery{GroupVersion: name + "/" + version, Version: version}
	return metav1.APIGroup{
		TypeMeta:         metav1.TypeMeta{APIVersion: "v1", Kind: "APIGroup"},
		Name:             name,
		Versions:         []metav1.GroupVersionForDiscovery{gv},
		PreferredVersion: gv,
	}
}

func fetchAPIResourceList() metav1.APIResourceList {
	return metav1.APIResourceList{
		TypeMeta:     metav1.TypeMeta{APIVersion: "v1", Kind: "APIResourceList"},
		GroupVersion: fetchv1alpha1.GroupVersion,
		APIResources: []metav1.APIResource{
			{
				Name:         fetchv1alpha1.Plural,
				SingularName: fetchv1alpha1.Singular,
				Namespaced:   false,
				Kind:         fetchv1alpha1.Kind,
				Verbs:        []string{"create", "delete", "get", "list", "patch", "update", "watch"},
				ShortNames:   []string{"fk"},
			},
			{
				Name:       fetchv1alpha1.Plural + "/status",
				Namespaced: false,
				Kind:       fetchv1alpha1.Kind,
				Verbs:      []string{"get", "patch", "update"},
			},
		},
	}
}

func crdAPIResourceList() metav1.APIResourceList {
	return metav1.APIResourceList{
		TypeMeta:     metav1.TypeMeta{APIVersion: "v1", Kind: "APIResourceList"},
		GroupVersion: "apiextensions.k8s.io/v1",
		APIResources: []metav1.APIResource{
			{
				Name:         "customresourcedefinitions",
				SingularName: "customresourcedefinition",
				Namespaced:   false,
				Kind:         "CustomResourceDefinition",
				Verbs:        []string{"get", "list", "watch"},
				ShortNames:   []string{"crd", "crds"},
			},
		},
	}
}

func (s *Server) handleFetches(req *Request, rest string, q url.Values) *Response {
	rest = strings.Trim(rest, "/")
	statusSubresource := false
	var name string
	if rest != "" {
		parts := strings.Split(rest, "/")
		if len(parts) == 2 && parts[1] == "status" {
			statusSubresource = true
		} else if len(parts) != 1 {
			return errorResponse(http.StatusNotFound, metav1.StatusReasonNotFound, "unknown path")
		}
		name = parts[0]
	}

	switch {
	case name == "" && req.Method == http.MethodGet:
		return s.listFetches(req, q)
	case name == "" && req.Method == http.MethodPost:
		return s.createFetch(req)
	case name != "" && statusSubresource && req.Method == http.MethodGet:
		return s.getFetch(name, req)
	case name != "" && statusSubresource && (req.Method == http.MethodPut || req.Method == http.MethodPatch):
		return s.updateFetch(name, req, true)
	case name != "" && req.Method == http.MethodGet:
		return s.getFetch(name, req)
	case name != "" && req.Method == http.MethodPut:
		return s.updateFetch(name, req, false)
	case name != "" && req.Method == http.MethodPatch:
		return s.updateFetch(name, req, false)
	case name != "" && req.Method == http.MethodDelete:
		return s.deleteFetch(name)
	default:
		return errorResponse(http.StatusMethodNotAllowed, metav1.StatusReasonMethodNotAllowed, "method "+req.Method+" is not supported for "+req.Path)
	}
}

func (s *Server) createFetch(req *Request) *Response {
	var in fetchv1alpha1.Fetch
	if err := json.Unmarshal([]byte(req.Body), &in); err != nil {
		return errorResponse(http.StatusBadRequest, metav1.StatusReasonBadRequest, "invalid Fetch body: "+err.Error())
	}

	out, apiErr := s.create(&in)
	if apiErr != nil {
		return statusResponse(apiErr)
	}
	return jsonResponse(http.StatusCreated, out)
}

func (s *Server) create(in *fetchv1alpha1.Fetch) (*fetchv1alpha1.Fetch, *metav1.Status) {
	if in.Name == "" {
		if in.GenerateName == "" {
			return nil, statusErr(http.StatusUnprocessableEntity, metav1.StatusReasonInvalid, "metadata.name or metadata.generateName is required")
		}
		in.Name = in.GenerateName + randSuffix()
	}

	now := metav1.Now()
	out := in.DeepCopy()
	out.TypeMeta = metav1.TypeMeta{APIVersion: fetchv1alpha1.GroupVersion, Kind: fetchv1alpha1.Kind}
	out.UID = types.UID(newUID())
	out.CreationTimestamp = now
	out.Generation = 1
	out.ResourceVersion = ""
	out.Status = fetchv1alpha1.FetchStatus{Phase: fetchv1alpha1.FetchPhaseUnresolved}

	var stored fetchv1alpha1.Fetch
	if err := s.store.Create(context.Background(), fetchKey(out.Name), out, &stored, 0); err != nil {
		if kstorage.IsExist(err) {
			return nil, statusErr(http.StatusConflict, metav1.StatusReasonAlreadyExists, fmt.Sprintf("%s %q already exists", fetchv1alpha1.Plural, out.Name))
		}
		return nil, statusErr(http.StatusInternalServerError, metav1.StatusReasonInternalError, err.Error())
	}

	normalize(&stored)
	s.enqueue(out.Name)
	return &stored, nil
}

func (s *Server) getFetch(name string, req *Request) *Response {
	ctx := context.Background()
	var out fetchv1alpha1.Fetch
	if err := s.store.Get(ctx, fetchKey(name), kstorage.GetOptions{}, &out); err != nil {
		return storageErrorResponse(err)
	}
	normalize(&out)

	if wantsTable(req.Headers) {
		rv, _ := s.store.GetCurrentResourceVersion(ctx)
		return jsonResponse(http.StatusOK, s.toTable([]*fetchv1alpha1.Fetch{&out}, int64(rv)))
	}
	return jsonResponse(http.StatusOK, &out)
}

func (s *Server) listFetches(req *Request, q url.Values) *Response {
	opts, apiErr := listOptions(q)
	if apiErr != nil {
		return statusResponse(apiErr)
	}

	var list fetchv1alpha1.FetchList
	if err := s.store.GetList(context.Background(), fetchStorePrefix, opts, &list); err != nil {
		return storageErrorResponse(err)
	}
	list.TypeMeta = metav1.TypeMeta{APIVersion: fetchv1alpha1.GroupVersion, Kind: fetchv1alpha1.ListKind}

	for i := range list.Items {
		normalize(&list.Items[i])
	}

	if wantsTable(req.Headers) {
		items := make([]*fetchv1alpha1.Fetch, 0, len(list.Items))
		for i := range list.Items {
			items = append(items, &list.Items[i])
		}
		rv, _ := strconv.ParseInt(list.ResourceVersion, 10, 64)
		return jsonResponse(http.StatusOK, s.toTable(items, rv))
	}
	return jsonResponse(http.StatusOK, &list)
}

func listOptions(q url.Values) (kstorage.ListOptions, *metav1.Status) {
	predicate := kstorage.SelectionPredicate{
		Label: labels.Everything(),
		Field: fields.Everything(),
	}
	if raw := q.Get("labelSelector"); raw != "" {
		sel, err := labels.Parse(raw)
		if err != nil {
			return kstorage.ListOptions{}, statusErr(http.StatusBadRequest, metav1.StatusReasonBadRequest, "invalid labelSelector: "+err.Error())
		}
		predicate.Label = sel
	}
	if raw := q.Get("fieldSelector"); raw != "" {
		sel, err := fields.ParseSelector(raw)
		if err != nil {
			return kstorage.ListOptions{}, statusErr(http.StatusBadRequest, metav1.StatusReasonBadRequest, "invalid fieldSelector: "+err.Error())
		}
		predicate.Field = sel
	}
	return kstorage.ListOptions{Predicate: predicate, Recursive: true}, nil
}

func (s *Server) updateFetch(name string, req *Request, statusOnly bool) *Response {
	contentType := header(req.Headers, "content-type")
	isPatch := strings.Contains(contentType, "patch")

	var patchValue any
	var in fetchv1alpha1.Fetch
	if isPatch {
		val, err := parsePatch([]byte(req.Body), contentType)
		if err != nil {
			return errorResponse(http.StatusBadRequest, metav1.StatusReasonBadRequest, err.Error())
		}
		patchValue = val
	} else if err := json.Unmarshal([]byte(req.Body), &in); err != nil {
		return errorResponse(http.StatusBadRequest, metav1.StatusReasonBadRequest, "invalid Fetch body: "+err.Error())
	}

	enqueue := false
	var out fetchv1alpha1.Fetch
	err := s.store.GuaranteedUpdate(context.Background(), fetchKey(name), &out, false, nil,
		func(input runtime.Object, _ kstorage.ResponseMeta) (runtime.Object, *uint64, error) {
			current := input.(*fetchv1alpha1.Fetch)

			desired := in
			if isPatch {
				patched, err := applyMergePatch(current, patchValue)
				if err != nil {
					return nil, nil, err
				}
				desired = *patched
			}

			updated := current.DeepCopy()
			if statusOnly {
				updated.Status = *desired.Status.DeepCopy()
			} else {
				specChanged := current.Spec != desired.Spec
				updated.Spec = desired.Spec
				// Mutations to the mutable parts of metadata must be persisted
				// too; kubectl apply stores its last-applied configuration as
				// an annotation on every update.
				if desired.Labels != nil {
					updated.Labels = desired.Labels
				}
				if desired.Annotations != nil {
					updated.Annotations = desired.Annotations
				}
				if desired.Finalizers != nil {
					updated.Finalizers = desired.Finalizers
				}
				if specChanged {
					updated.Generation = current.Generation + 1
					updated.Status = fetchv1alpha1.FetchStatus{Phase: fetchv1alpha1.FetchPhaseUnresolved}
					enqueue = updated.Spec.URL != ""
				}
			}

			updated.UID = current.UID
			updated.CreationTimestamp = current.CreationTimestamp
			normalize(updated)
			return updated, nil, nil
		}, nil)
	if err != nil {
		return storageErrorResponse(err)
	}

	normalize(&out)
	if enqueue {
		s.enqueue(name)
	}
	return jsonResponse(http.StatusOK, &out)
}

// parsePatch turns a merge/apply patch body (JSON or YAML) into a value
// suitable for mergePatch.
func parsePatch(body []byte, contentType string) (any, error) {
	patchJSON := body
	if strings.Contains(contentType, "yaml") {
		var err error
		patchJSON, err = yaml.YAMLToJSON(body)
		if err != nil {
			return nil, fmt.Errorf("invalid YAML patch: %w", err)
		}
	}
	var value any
	if err := json.Unmarshal(patchJSON, &value); err != nil {
		return nil, fmt.Errorf("invalid patch: %w", err)
	}
	return value, nil
}

func applyMergePatch(current *fetchv1alpha1.Fetch, patchValue any) (*fetchv1alpha1.Fetch, error) {
	rawCurrent, err := json.Marshal(current)
	if err != nil {
		return nil, err
	}
	var currentValue any
	if err := json.Unmarshal(rawCurrent, &currentValue); err != nil {
		return nil, err
	}
	merged, err := json.Marshal(mergePatch(currentValue, patchValue))
	if err != nil {
		return nil, err
	}
	var result fetchv1alpha1.Fetch
	if err := json.Unmarshal(merged, &result); err != nil {
		return nil, fmt.Errorf("patched object is invalid: %w", err)
	}
	return &result, nil
}

func (s *Server) deleteFetch(name string) *Response {
	var out fetchv1alpha1.Fetch
	if err := s.store.Delete(context.Background(), fetchKey(name), &out, nil, nil, nil, kstorage.DeleteOptions{}); err != nil {
		return storageErrorResponse(err)
	}
	return jsonResponse(http.StatusOK, metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   metav1.StatusSuccess,
		Details:  &metav1.StatusDetails{Name: name, Group: fetchv1alpha1.GroupName, Kind: fetchv1alpha1.Plural},
	})
}

func (s *Server) handleCRDList(req *Request) *Response {
	crd := fetchv1alpha1.NewCRD()
	list := &metav1.PartialObjectMetadataList{}
	list.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "PartialObjectMetadataList"}
	list.Items = []metav1.PartialObjectMetadata{{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apiextensions.k8s.io/v1", Kind: "CustomResourceDefinition"},
		ObjectMeta: crd.ObjectMeta,
	}}
	return jsonResponse(http.StatusOK, list)
}

func (s *Server) handleCRD(name string) *Response {
	crd := fetchv1alpha1.NewCRD()
	if name != crd.Name {
		return notFound(name)
	}
	return jsonResponse(http.StatusOK, crd)
}

func normalize(f *fetchv1alpha1.Fetch) {
	f.TypeMeta = metav1.TypeMeta{APIVersion: fetchv1alpha1.GroupVersion, Kind: fetchv1alpha1.Kind}
}

func wantsTable(headers map[string]string) bool {
	accept := header(headers, "accept")
	return strings.Contains(accept, "as=Table") || strings.Contains(accept, "as=table")
}

func header(headers map[string]string, key string) string {
	if headers == nil {
		return ""
	}
	if v, ok := headers[key]; ok {
		return v
	}
	for k, v := range headers {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}

func jsonResponse(code int, obj any) *Response {
	b, err := json.Marshal(obj)
	if err != nil {
		return errorResponse(http.StatusInternalServerError, metav1.StatusReasonInternalError, err.Error())
	}
	return &Response{
		Status:  code,
		Headers: map[string]string{"content-type": "application/json"},
		Body:    string(b),
	}
}

func textResponse(code int, body string) *Response {
	return &Response{
		Status:  code,
		Headers: map[string]string{"content-type": "text/plain; charset=utf-8"},
		Body:    body,
	}
}

func statusResponse(st *metav1.Status) *Response {
	return jsonResponse(int(st.Code), st)
}

func errorResponse(code int, reason metav1.StatusReason, message string) *Response {
	return statusResponse(statusErr(code, reason, message))
}

func statusErr(code int, reason metav1.StatusReason, message string) *metav1.Status {
	return &metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   metav1.StatusFailure,
		Message:  message,
		Reason:   reason,
		Code:     int32(code),
	}
}

func notFound(name string) *Response {
	return errorResponse(http.StatusNotFound, metav1.StatusReasonNotFound, fmt.Sprintf("%s %q not found", fetchv1alpha1.Plural, name))
}

// storageErrorResponse maps storage.Interface errors onto Kubernetes Status
// responses.
func storageErrorResponse(err error) *Response {
	switch {
	case kstorage.IsNotFound(err):
		return errorResponse(http.StatusNotFound, metav1.StatusReasonNotFound, err.Error())
	case kstorage.IsExist(err):
		return errorResponse(http.StatusConflict, metav1.StatusReasonAlreadyExists, err.Error())
	case kstorage.IsConflict(err):
		return errorResponse(http.StatusConflict, metav1.StatusReasonConflict, err.Error())
	case kstorage.IsInvalidObj(err), kstorage.IsInvalidError(err):
		return errorResponse(http.StatusUnprocessableEntity, metav1.StatusReasonInvalid, err.Error())
	default:
		return errorResponse(http.StatusInternalServerError, metav1.StatusReasonInternalError, err.Error())
	}
}

func randSuffix() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func newUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// mergePatch implements RFC 7386 JSON Merge Patch. It is used for both
// merge-patch and (client side) apply-patch requests issued by kubectl.
func mergePatch(dst, patch any) any {
	pm, ok := patch.(map[string]any)
	if !ok {
		return patch
	}
	dm, ok := dst.(map[string]any)
	if !ok {
		dm = map[string]any{}
	}
	for k, v := range pm {
		if v == nil {
			delete(dm, k)
			continue
		}
		dm[k] = mergePatch(dm[k], v)
	}
	return dm
}
