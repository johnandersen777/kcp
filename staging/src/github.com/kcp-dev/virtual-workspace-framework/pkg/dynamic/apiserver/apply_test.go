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

package apiserver

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"k8s.io/apimachinery/pkg/types"

	dynamiccontext "github.com/kcp-dev/virtual-workspace-framework/pkg/dynamic/context"
)

func TestCaptureApplyPatch(t *testing.T) {
	t.Parallel()

	const body = "apiVersion: v1\nkind: Thing\n"

	tests := map[string]struct {
		contentType string
		query       string
		maxBytes    int64
		wantCapture bool
		wantForce   bool
	}{
		"apply":                  {contentType: string(types.ApplyPatchType), query: "fieldManager=m", wantCapture: true},
		"apply with charset":     {contentType: string(types.ApplyPatchType) + "; charset=utf-8", query: "fieldManager=m", wantCapture: true},
		"apply with force":       {contentType: string(types.ApplyPatchType), query: "fieldManager=m&force=true", wantCapture: true, wantForce: true},
		"apply within the limit": {contentType: string(types.ApplyPatchType), maxBytes: int64(len(body)), wantCapture: true},
		"apply above the limit":  {contentType: string(types.ApplyPatchType), maxBytes: int64(len(body)) - 1},
		"merge patch":            {contentType: string(types.MergePatchType)},
		"json patch":             {contentType: string(types.JSONPatchType)},
		"apply with force=false": {contentType: string(types.ApplyPatchType), query: "fieldManager=m&force=false", wantCapture: true},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			var gotBody string
			var applyPatch *dynamiccontext.ApplyPatch
			var captured bool
			handler := CaptureApplyPatch(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				data, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				gotBody = string(data)
				applyPatch, captured = dynamiccontext.ApplyPatchFrom(req.Context())
			}), tc.maxBytes)

			req := httptest.NewRequest(http.MethodPatch, "/apis/g/v1/things/a?"+tc.query, strings.NewReader(body))
			req.Header.Set("Content-Type", tc.contentType)
			handler.ServeHTTP(httptest.NewRecorder(), req)

			require.Equal(t, body, gotBody, "the wrapped handler must get the whole body")
			require.Equal(t, tc.wantCapture, captured)
			if tc.wantCapture {
				require.Equal(t, body, string(applyPatch.Data))
				require.Equal(t, tc.wantForce, applyPatch.Force)
			}
		})
	}
}
