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

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	fetchv1alpha1 "github.com/kcp-dev/kcp/contrib/wasm/apis/fetch/v1alpha1"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	s := New()
	s.Start()
	t.Cleanup(s.Stop)
	return s
}

func do(t *testing.T, s *Server, method, path, body string, headers map[string]string) *Response {
	t.Helper()
	return s.Handle(&Request{Method: method, Path: path, Body: body, Headers: headers})
}

func decodeFetch(t *testing.T, resp *Response) *fetchv1alpha1.Fetch {
	t.Helper()
	var f fetchv1alpha1.Fetch
	if err := json.Unmarshal([]byte(resp.Body), &f); err != nil {
		t.Fatalf("decoding Fetch response (status %d): %v\nbody: %s", resp.Status, err, resp.Body)
	}
	return &f
}

func waitForPhase(t *testing.T, s *Server, name string, want fetchv1alpha1.FetchPhase) *fetchv1alpha1.Fetch {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last *fetchv1alpha1.Fetch
	for time.Now().Before(deadline) {
		resp := do(t, s, http.MethodGet, fetchPrefix+"/"+name, "", nil)
		if resp.Status != http.StatusOK {
			t.Fatalf("get %s: status %d body %s", name, resp.Status, resp.Body)
		}
		last = decodeFetch(t, resp)
		if last.Status.Phase == want {
			return last
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("Fetch %q did not reach phase %q, last phase %q (message %q)", name, want, last.Status.Phase, last.Status.Message)
	return nil
}

func TestFetchStartsUnresolvedAndStoresBody(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("hello from origin"))
	}))
	defer origin.Close()

	s := newTestServer(t)

	resp := do(t, s, http.MethodPost, fetchPrefix, `{
		"apiVersion":"fetch.kcp.io/v1alpha1",
		"kind":"Fetch",
		"metadata":{"name":"demo"},
		"spec":{"url":"`+origin.URL+`"}
	}`, map[string]string{"content-type": "application/json"})
	if resp.Status != http.StatusCreated {
		t.Fatalf("create: status %d body %s", resp.Status, resp.Body)
	}
	created := decodeFetch(t, resp)
	if created.Status.Phase != fetchv1alpha1.FetchPhaseUnresolved && created.Status.Phase != fetchv1alpha1.FetchPhaseFetching && created.Status.Phase != fetchv1alpha1.FetchPhaseResolved {
		t.Fatalf("unexpected initial phase %q", created.Status.Phase)
	}

	f := waitForPhase(t, s, "demo", fetchv1alpha1.FetchPhaseResolved)
	if f.Status.Body != "hello from origin" {
		t.Errorf("body = %q, want %q", f.Status.Body, "hello from origin")
	}
	if f.Status.StatusCode != http.StatusOK {
		t.Errorf("statusCode = %d, want 200", f.Status.StatusCode)
	}
	if f.Status.ContentType != "text/plain" {
		t.Errorf("contentType = %q, want text/plain", f.Status.ContentType)
	}
	if f.Status.FetchedAt == nil {
		t.Error("fetchedAt is nil")
	}
}

func TestFetchWithoutURLStaysUnresolvedUntilUpdated(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("late body"))
	}))
	defer origin.Close()

	s := newTestServer(t)

	resp := do(t, s, http.MethodPost, fetchPrefix, `{
		"apiVersion":"fetch.kcp.io/v1alpha1",
		"kind":"Fetch",
		"metadata":{"name":"later"},
		"spec":{}
	}`, nil)
	if resp.Status != http.StatusCreated {
		t.Fatalf("create: status %d body %s", resp.Status, resp.Body)
	}

	time.Sleep(150 * time.Millisecond)
	f := decodeFetch(t, do(t, s, http.MethodGet, fetchPrefix+"/later", "", nil))
	if f.Status.Phase != fetchv1alpha1.FetchPhaseUnresolved {
		t.Fatalf("phase = %q, want Unresolved", f.Status.Phase)
	}

	patch := `{"spec":{"url":"` + origin.URL + `"}}`
	resp = do(t, s, http.MethodPatch, fetchPrefix+"/later", patch, map[string]string{"content-type": "application/merge-patch+json"})
	if resp.Status != http.StatusOK {
		t.Fatalf("patch: status %d body %s", resp.Status, resp.Body)
	}

	f = waitForPhase(t, s, "later", fetchv1alpha1.FetchPhaseResolved)
	if f.Status.Body != "late body" {
		t.Errorf("body = %q, want %q", f.Status.Body, "late body")
	}
	if f.Generation != 2 {
		t.Errorf("generation = %d, want 2", f.Generation)
	}
}

func TestFetchErrorPhaseStillStoresBody(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("short and stout"))
	}))
	defer origin.Close()

	s := newTestServer(t)
	do(t, s, http.MethodPost, fetchPrefix, `{"metadata":{"name":"teapot"},"spec":{"url":"`+origin.URL+`"}}`, nil)

	f := waitForPhase(t, s, "teapot", fetchv1alpha1.FetchPhaseError)
	if f.Status.StatusCode != http.StatusTeapot {
		t.Errorf("statusCode = %d, want 418", f.Status.StatusCode)
	}
	if f.Status.Body != "short and stout" {
		t.Errorf("body = %q, want %q", f.Status.Body, "short and stout")
	}
	if !strings.Contains(f.Status.Message, "418") {
		t.Errorf("message = %q, want it to mention 418", f.Status.Message)
	}
}

func TestTableIncludesBodyColumn(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("body-in-table"))
	}))
	defer origin.Close()

	s := newTestServer(t)
	do(t, s, http.MethodPost, fetchPrefix, `{"metadata":{"name":"tabled"},"spec":{"url":"`+origin.URL+`"}}`, nil)
	waitForPhase(t, s, "tabled", fetchv1alpha1.FetchPhaseResolved)

	headers := map[string]string{"accept": "application/json;as=Table;v=v1;g=meta.k8s.io"}
	resp := do(t, s, http.MethodGet, fetchPrefix, "", headers)
	if resp.Status != http.StatusOK {
		t.Fatalf("list: status %d body %s", resp.Status, resp.Body)
	}

	var tbl struct {
		Kind              string `json:"kind"`
		ColumnDefinitions []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"columnDefinitions"`
		Rows []struct {
			Cells []any `json:"cells"`
		} `json:"rows"`
	}
	if err := json.Unmarshal([]byte(resp.Body), &tbl); err != nil {
		t.Fatalf("decoding table: %v", err)
	}
	if tbl.Kind != "Table" {
		t.Fatalf("kind = %q, want Table", tbl.Kind)
	}

	bodyIdx := -1
	for i, c := range tbl.ColumnDefinitions {
		if c.Name == "Body" {
			bodyIdx = i
		}
	}
	if bodyIdx < 0 {
		t.Fatalf("no Body column in %+v", tbl.ColumnDefinitions)
	}
	if len(tbl.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(tbl.Rows))
	}
	if got := tbl.Rows[0].Cells[bodyIdx]; got != "body-in-table" {
		t.Errorf("Body cell = %v, want body-in-table", got)
	}
}

func TestPatchPersistsMetadata(t *testing.T) {
	s := newTestServer(t)
	do(t, s, http.MethodPost, fetchPrefix, `{"metadata":{"name":"meta"},"spec":{}}`, nil)

	patch := `{"metadata":{"labels":{"team":"wasm"},"annotations":{"note":"kept"}}}`
	resp := do(t, s, http.MethodPatch, fetchPrefix+"/meta", patch, map[string]string{
		"content-type": "application/merge-patch+json",
	})
	if resp.Status != http.StatusOK {
		t.Fatalf("patch: status %d body %s", resp.Status, resp.Body)
	}
	f := decodeFetch(t, resp)
	if f.Labels["team"] != "wasm" {
		t.Errorf("labels = %v, want team=wasm", f.Labels)
	}
	if f.Annotations["note"] != "kept" {
		t.Errorf("annotations = %v, want note=kept", f.Annotations)
	}
	if f.Generation != 1 {
		t.Errorf("metadata-only patch bumped generation to %d, want 1", f.Generation)
	}
}

func TestApplyPatchYAMLCreatesUpdate(t *testing.T) {
	s := newTestServer(t)

	body := `apiVersion: fetch.kcp.io/v1alpha1
kind: Fetch
metadata:
  name: applied
spec:
  url: http://127.0.0.1:1/never-called
`
	resp := do(t, s, http.MethodPatch, fetchPrefix+"/applied", body, map[string]string{
		"content-type": "application/apply-patch+yaml",
	})
	// The object does not exist yet: kubectl would have POSTed instead. We
	// only assert that a missing object is reported, not patched.
	if resp.Status != http.StatusNotFound {
		t.Fatalf("patch missing object: status %d body %s", resp.Status, resp.Body)
	}

	do(t, s, http.MethodPost, fetchPrefix, `{"metadata":{"name":"applied"},"spec":{}}`, nil)
	body = `apiVersion: fetch.kcp.io/v1alpha1
kind: Fetch
metadata:
  name: applied
spec:
  url: http://127.0.0.1:1/never-called
`
	resp = do(t, s, http.MethodPatch, fetchPrefix+"/applied", body, map[string]string{
		"content-type": "application/apply-patch+yaml",
	})
	if resp.Status != http.StatusOK {
		t.Fatalf("apply patch: status %d body %s", resp.Status, resp.Body)
	}
	f := decodeFetch(t, resp)
	if f.Spec.URL != "http://127.0.0.1:1/never-called" {
		t.Errorf("url = %q", f.Spec.URL)
	}
}

func TestDiscoveryServesFetchCRD(t *testing.T) {
	s := newTestServer(t)

	resp := do(t, s, http.MethodGet, "/apis", "", nil)
	if resp.Status != http.StatusOK || !strings.Contains(resp.Body, fetchv1alpha1.GroupName) {
		t.Fatalf("/apis: status %d body %s", resp.Status, resp.Body)
	}

	resp = do(t, s, http.MethodGet, "/apis/"+fetchv1alpha1.GroupVersion, "", nil)
	if !strings.Contains(resp.Body, `"name":"fetches"`) {
		t.Fatalf("discovery does not announce fetches: %s", resp.Body)
	}

	resp = do(t, s, http.MethodGet, crdPath+"/"+fetchv1alpha1.CRDName, "", nil)
	if resp.Status != http.StatusOK {
		t.Fatalf("get CRD: status %d body %s", resp.Status, resp.Body)
	}
	if !strings.Contains(resp.Body, `"kind":"Fetch"`) || !strings.Contains(resp.Body, `"name":"Body"`) {
		t.Fatalf("CRD object is missing kind/columns: %s", resp.Body)
	}
}

func TestOpenAPIV2Protobuf(t *testing.T) {
	s := newTestServer(t)
	resp := do(t, s, http.MethodGet, "/openapi/v2", "", map[string]string{
		"accept": "application/com.github.proto-openapi.spec.v2@v1.0+protobuf",
	})
	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d body %s", resp.Status, resp.Body)
	}
	if resp.BodyBase64 == "" {
		t.Fatal("expected base64 protobuf body")
	}
	if got := resp.Headers["content-type"]; got != openAPIV2ContentType {
		t.Errorf("content-type = %q, want %q", got, openAPIV2ContentType)
	}
}
