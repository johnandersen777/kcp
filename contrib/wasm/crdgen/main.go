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

// Command crdgen prints the Fetch CustomResourceDefinition (defined in Go) as
// YAML, for applying it to a running kcp server.
package main

import (
	"fmt"
	"os"

	"sigs.k8s.io/yaml"

	fetchv1alpha1 "github.com/kcp-dev/kcp/contrib/wasm/apis/fetch/v1alpha1"
)

func main() {
	out, err := yaml.Marshal(fetchv1alpha1.NewCRD())
	if err != nil {
		fmt.Fprintln(os.Stderr, "marshaling CRD:", err)
		os.Exit(1)
	}
	fmt.Print(string(out))
}
