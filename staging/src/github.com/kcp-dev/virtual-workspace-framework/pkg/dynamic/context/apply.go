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

package context

import (
	"context"
	"sync"

	"k8s.io/apimachinery/pkg/runtime"
)

// applyPatchContextKeyType is the type of the key for the request context value
// that carries the raw server-side apply patch of a PATCH request.
type applyPatchContextKeyType string

// applyPatchContextKey is the key for the request context value
// that carries the raw server-side apply patch of a PATCH request.
const applyPatchContextKey applyPatchContextKeyType = "VirtualWorkspaceApplyPatch"

// ApplyPatch is the raw body and the force flag of a server-side apply
// (application/apply-patch+yaml) PATCH request served by a virtual workspace.
//
// The generic PATCH handler computes the applied object locally and hands it to
// the storage as an Update. A forwarding storage uses this value to send the
// apply itself to the backend instead, so that the backend records the field
// manager with operation Apply.
type ApplyPatch struct {
	// Data is the request body, as sent by the client.
	Data []byte
	// Force is the value of the force query parameter.
	Force bool

	mu             sync.Mutex
	admittedBefore runtime.Object
	admittedAfter  runtime.Object
}

// RecordAdmission records the object before and after mutating admission.
// Over several calls it keeps the first before and the last after.
func (p *ApplyPatch) RecordAdmission(before, after runtime.Object) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.admittedBefore == nil {
		p.admittedBefore = before
	}
	p.admittedAfter = after
}

// ResetAdmission forgets what RecordAdmission recorded, e.g. before a retry.
func (p *ApplyPatch) ResetAdmission() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.admittedBefore, p.admittedAfter = nil, nil
}

// Admission returns the object before and after mutating admission, if
// mutating admission ran since the last ResetAdmission.
func (p *ApplyPatch) Admission() (before, after runtime.Object, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.admittedBefore, p.admittedAfter, p.admittedBefore != nil && p.admittedAfter != nil
}

// WithApplyPatch adds a server-side apply patch to the context.
func WithApplyPatch(ctx context.Context, applyPatch *ApplyPatch) context.Context {
	return context.WithValue(ctx, applyPatchContextKey, applyPatch)
}

// ApplyPatchFrom retrieves the server-side apply patch from the context, if any.
func ApplyPatchFrom(ctx context.Context) (*ApplyPatch, bool) {
	applyPatch, ok := ctx.Value(applyPatchContextKey).(*ApplyPatch)
	return applyPatch, ok && applyPatch != nil
}
