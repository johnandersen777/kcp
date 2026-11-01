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
	"bytes"
	"io"
	"net/http"
	"strings"

	metainternalversionscheme "k8s.io/apimachinery/pkg/apis/meta/internalversion/scheme"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/endpoints/handlers"
	"k8s.io/apiserver/pkg/registry/rest"

	dynamiccontext "github.com/kcp-dev/virtual-workspace-framework/pkg/dynamic/context"
)

// PatchResource is the virtual workspace's PATCH handler: handlers.PatchResource
// wrapped with CaptureApplyPatch.
func PatchResource(storage rest.Patcher, scope *handlers.RequestScope, admit admission.Interface, patchTypes []string) http.HandlerFunc {
	return CaptureApplyPatch(handlers.PatchResource(storage, scope, admit, patchTypes), scope.MaxRequestBodyBytes)
}

// CaptureApplyPatch wraps a PATCH handler (handlers.PatchResource). For a
// server-side apply request (application/apply-patch+yaml) it reads the body,
// gives it back to the wrapped handler unchanged, and stores it together with
// the force parameter in the request context (dynamiccontext.WithApplyPatch),
// so that a forwarding storage can send the apply itself to the backend
// instead of the object the handler computed locally.
//
// Anything it cannot capture (another patch type, a body above
// maxRequestBodyBytes, a read error, invalid query parameters) is passed
// through untouched: the wrapped handler then answers exactly as before.
func CaptureApplyPatch(handler http.Handler, maxRequestBodyBytes int64) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		contentType := req.Header.Get("Content-Type")
		if idx := strings.Index(contentType, ";"); idx > 0 {
			contentType = contentType[:idx]
		}
		if types.PatchType(contentType) != types.ApplyPatchType || req.Body == nil {
			handler.ServeHTTP(w, req)
			return
		}

		options := &metav1.PatchOptions{}
		if err := metainternalversionscheme.ParameterCodec.DecodeParameters(req.URL.Query(), metav1.SchemeGroupVersion, options); err != nil {
			handler.ServeHTTP(w, req)
			return
		}

		body := req.Body
		var reader io.Reader = body
		if maxRequestBodyBytes > 0 {
			reader = io.LimitReader(body, maxRequestBodyBytes+1)
		}
		data, err := io.ReadAll(reader)
		if err != nil || (maxRequestBodyBytes > 0 && int64(len(data)) > maxRequestBodyBytes) {
			// Hand the wrapped handler what was read plus the rest, so it
			// reports the error (e.g. 413) the same way it always did.
			req.Body = readCloser{Reader: io.MultiReader(bytes.NewReader(data), body), Closer: body}
			handler.ServeHTTP(w, req)
			return
		}
		req.Body = readCloser{Reader: bytes.NewReader(data), Closer: body}

		applyPatch := &dynamiccontext.ApplyPatch{
			Data:  data,
			Force: options.Force != nil && *options.Force,
		}
		handler.ServeHTTP(w, req.WithContext(dynamiccontext.WithApplyPatch(req.Context(), applyPatch)))
	}
}

type readCloser struct {
	io.Reader
	io.Closer
}
