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

// Package v1alpha1 contains the Fetch custom resource, a tiny CRD used to
// demonstrate that a kcp-compatible API server running inside a WASM module
// can perform outbound HTTP requests through its Deno host.
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	// GroupName is the API group of the Fetch CRD.
	GroupName = "fetch.kcp.io"
	// Version is the API version of the Fetch CRD.
	Version = "v1alpha1"
	// GroupVersion is the combined group/version.
	GroupVersion = GroupName + "/" + Version
	// Kind is the kind of the Fetch CRD.
	Kind = "Fetch"
	// Plural is the resource plural.
	Plural = "fetches"
	// Singular is the resource singular.
	Singular = "fetch"
	// ListKind is the list kind.
	ListKind = "FetchList"
)

// FetchPhase is the lifecycle phase of a Fetch.
type FetchPhase string

const (
	// FetchPhaseUnresolved is the initial phase: the Fetch has not been
	// reconciled yet (or has no URL to fetch).
	FetchPhaseUnresolved FetchPhase = "Unresolved"
	// FetchPhaseFetching is set while the request is in flight.
	FetchPhaseFetching FetchPhase = "Fetching"
	// FetchPhaseResolved means the URL was fetched successfully. The
	// response body is stored in status.body.
	FetchPhaseResolved FetchPhase = "Resolved"
	// FetchPhaseError means the fetch failed.
	FetchPhaseError FetchPhase = "Error"
)

// FetchSpec describes what to fetch.
type FetchSpec struct {
	// URL is the location to fetch. When empty the Fetch remains Unresolved.
	URL string `json:"url,omitempty"`
	// Method is the HTTP method to use. Defaults to GET.
	Method string `json:"method,omitempty"`
}

// FetchStatus is the observed state of a Fetch.
type FetchStatus struct {
	// Phase is the lifecycle phase: Unresolved, Fetching, Resolved or Error.
	Phase FetchPhase `json:"phase,omitempty"`
	// StatusCode is the HTTP status code returned by the URL.
	StatusCode int `json:"statusCode,omitempty"`
	// Body is the response body (up to 1MiB). It is exposed as a printer
	// column so that it is visible in `kubectl get fetches`.
	Body string `json:"body,omitempty"`
	// ContentType is the response Content-Type header.
	ContentType string `json:"contentType,omitempty"`
	// Message carries error details or truncation notices.
	Message string `json:"message,omitempty"`
	// ObservedGeneration is the metadata.generation the controller last acted on.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// FetchedAt is the time of the last fetch attempt.
	FetchedAt *metav1.Time `json:"fetchedAt,omitempty"`
}

// Fetch is a request to fetch a URL. Its body is stored in status.body.
type Fetch struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FetchSpec   `json:"spec,omitempty"`
	Status FetchStatus `json:"status,omitempty"`
}

// FetchList is a list of Fetch objects.
type FetchList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Fetch `json:"items"`
}

// DeepCopyInto copies the receiver into out.
func (in *Fetch) DeepCopyInto(out *Fetch) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	in.Status.DeepCopyInto(&out.Status)
}

// DeepCopy returns a deep copy of the Fetch.
func (in *Fetch) DeepCopy() *Fetch {
	if in == nil {
		return nil
	}
	out := new(Fetch)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *Fetch) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}

// DeepCopyInto copies the receiver into out.
func (in *FetchStatus) DeepCopyInto(out *FetchStatus) {
	*out = *in
	if in.FetchedAt != nil {
		out.FetchedAt = in.FetchedAt.DeepCopy()
	}
}

// DeepCopy returns a deep copy of the status.
func (in *FetchStatus) DeepCopy() *FetchStatus {
	if in == nil {
		return nil
	}
	out := new(FetchStatus)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out.
func (in *FetchList) DeepCopyInto(out *FetchList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]Fetch, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

// DeepCopy returns a deep copy of the list.
func (in *FetchList) DeepCopy() *FetchList {
	if in == nil {
		return nil
	}
	out := new(FetchList)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyObject implements runtime.Object.
func (in *FetchList) DeepCopyObject() runtime.Object {
	return in.DeepCopy()
}
