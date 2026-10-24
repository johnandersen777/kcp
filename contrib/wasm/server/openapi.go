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
	"encoding/base64"

	openapi_v2 "github.com/google/gnostic-models/openapiv2"
	"google.golang.org/protobuf/proto"
)

// openAPIV2ContentType is the media type the Kubernetes OpenAPI handler uses
// in responses. Note that kubectl *requests* the deprecated spelling with an
// "@" (application/com.github.proto-openapi.spec.v2@v1.0+protobuf); the
// response must use the modern, MIME-valid spelling.
const openAPIV2ContentType = "application/com.github.proto-openapi.spec.v2.v1.0+protobuf"

// openAPIV2Protobuf returns a minimal, well-formed OpenAPI v2 document in
// protobuf form. kubectl unconditionally proto-decodes this endpoint, so
// returning JSON would fail with "cannot parse invalid wire-format data".
// With no definition for the Fetch kind, kubectl skips schema validation for
// it and proceeds with the apply.
func openAPIV2Protobuf() (*Response, error) {
	doc := &openapi_v2.Document{
		Swagger: "2.0",
		Info: &openapi_v2.Info{
			Title:   "kcp-wasm",
			Version: "v1.37.0",
		},
		Paths:       &openapi_v2.Paths{},
		Definitions: &openapi_v2.Definitions{},
	}
	raw, err := proto.Marshal(doc)
	if err != nil {
		return nil, err
	}
	return &Response{
		Status:     200,
		Headers:    map[string]string{"content-type": openAPIV2ContentType},
		BodyBase64: base64.StdEncoding.EncodeToString(raw),
	}, nil
}
