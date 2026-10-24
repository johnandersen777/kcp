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

// Slim kubectl overlay for k8s.io/cli-runtime/pkg/printers/template.go.
//
// text/template evaluates fields with reflect.Value.MethodByName, which makes
// the Go linker keep every exported method of every reachable type (all typed
// clientsets, all API group deep-copy and protobuf methods, ...). Dropping the
// go-template printer lets the linker's method dead-code elimination run.

package printers

import (
	"errors"
	"io"

	"k8s.io/apimachinery/pkg/runtime"
)

// GoTemplatePrinter is unavailable in the mobile kubectl.
type GoTemplatePrinter struct{}

func NewGoTemplatePrinter(tmpl []byte) (*GoTemplatePrinter, error) {
	return nil, errors.New("-o go-template is not available in the mobile kubectl; use -o jsonpath or -o json")
}

func (p *GoTemplatePrinter) AllowMissingKeys(allow bool) {}

func (p *GoTemplatePrinter) PrintObj(obj runtime.Object, w io.Writer) error {
	return errors.New("-o go-template is not available in the mobile kubectl")
}
