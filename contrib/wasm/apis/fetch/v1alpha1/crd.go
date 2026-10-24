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

package v1alpha1

import (
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CRDName is the full name of the Fetch CustomResourceDefinition.
const CRDName = Plural + "." + GroupName

// NewCRD returns the Fetch CustomResourceDefinition, written in Go. The WASM
// kcp shard serves this object under apiextensions.k8s.io/v1 and derives its
// table columns from spec.versions[0].additionalPrinterColumns, so the CRD
// definition is the single source of truth for what `kubectl get fetches`
// displays.
func NewCRD() *apiextensionsv1.CustomResourceDefinition {
	jsonString := func(s string) apiextensionsv1.JSON {
		return apiextensionsv1.JSON{Raw: []byte(`"` + s + `"`)}
	}

	return &apiextensionsv1.CustomResourceDefinition{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apiextensions.k8s.io/v1",
			Kind:       "CustomResourceDefinition",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: CRDName,
			Annotations: map[string]string{
				"controller-gen.kubebuilder.io/version": "kcp-wasm",
			},
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: GroupName,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural:     Plural,
				Singular:   Singular,
				Kind:       Kind,
				ListKind:   ListKind,
				ShortNames: []string{"fk"},
			},
			Scope: apiextensionsv1.ClusterScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{
					Name:    Version,
					Served:  true,
					Storage: true,
					Subresources: &apiextensionsv1.CustomResourceSubresources{
						Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
					},
					AdditionalPrinterColumns: []apiextensionsv1.CustomResourceColumnDefinition{
						{
							Name:     "URL",
							Type:     "string",
							JSONPath: ".spec.url",
						},
						{
							Name:     "Phase",
							Type:     "string",
							JSONPath: ".status.phase",
						},
						{
							Name:     "Code",
							Type:     "integer",
							JSONPath: ".status.statusCode",
						},
						{
							Name:        "Body",
							Type:        "string",
							Description: "The response body stored by the controller.",
							JSONPath:    ".status.body",
						},
						{
							Name:     "Age",
							Type:     "date",
							JSONPath: ".metadata.creationTimestamp",
						},
					},
					Schema: &apiextensionsv1.CustomResourceValidation{
						OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
							Type:        "object",
							Description: "Fetch requests a URL and stores the response body in status.body.",
							Properties: map[string]apiextensionsv1.JSONSchemaProps{
								"spec": {
									Type: "object",
									Properties: map[string]apiextensionsv1.JSONSchemaProps{
										"url": {
											Type:        "string",
											Description: "The URL to fetch. While empty the Fetch stays Unresolved.",
										},
										"method": {
											Type:        "string",
											Description: "HTTP method to use. Defaults to GET.",
											Default:     &[]apiextensionsv1.JSON{jsonString("GET")}[0],
										},
									},
								},
								"status": {
									Type: "object",
									Properties: map[string]apiextensionsv1.JSONSchemaProps{
										"phase": {
											Type:        "string",
											Description: "Lifecycle phase of the Fetch.",
											Enum: []apiextensionsv1.JSON{
												jsonString(string(FetchPhaseUnresolved)),
												jsonString(string(FetchPhaseFetching)),
												jsonString(string(FetchPhaseResolved)),
												jsonString(string(FetchPhaseError)),
											},
										},
										"statusCode": {
											Type:        "integer",
											Description: "HTTP status code returned by the URL.",
										},
										"body": {
											Type:        "string",
											Description: "Response body (up to 1MiB).",
										},
										"contentType": {
											Type:        "string",
											Description: "Response Content-Type.",
										},
										"message": {
											Type:        "string",
											Description: "Error details or truncation notice.",
										},
										"observedGeneration": {
											Type:        "integer",
											Description: "Generation observed by the controller.",
										},
										"fetchedAt": {
											Type:   "string",
											Format: "date-time",
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}
