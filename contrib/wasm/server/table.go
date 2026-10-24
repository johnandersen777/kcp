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
	"fmt"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	fetchv1alpha1 "github.com/kcp-dev/kcp/contrib/wasm/apis/fetch/v1alpha1"
)

// Table types from meta.k8s.io/v1. kubectl requests these via content
// negotiation (Accept: application/json;as=Table;...). Serving a Table lets
// the CRD's additionalPrinterColumns — including the response BODY — show up
// directly in `kubectl get fetches`.
type table struct {
	Kind              string                  `json:"kind"`
	APIVersion        string                  `json:"apiVersion"`
	Metadata          metav1.ListMeta         `json:"metadata,omitempty"`
	ColumnDefinitions []tableColumnDefinition `json:"columnDefinitions"`
	Rows              []tableRow              `json:"rows"`
}

type tableColumnDefinition struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Format      string `json:"format,omitempty"`
	Description string `json:"description,omitempty"`
	Priority    int32  `json:"priority"`
}

type tableRow struct {
	Cells  []any           `json:"cells"`
	Object json.RawMessage `json:"object,omitempty"`
}

type tableColumn struct {
	def  tableColumnDefinition
	path string
}

// toTable converts Fetch objects to a meta.k8s.io/v1 Table. The columns are
// taken from the Go-defined CRD so the CRD remains the source of truth for
// what kubectl displays.
func (s *Server) toTable(items []*fetchv1alpha1.Fetch, resourceVersion int64) *table {
	cols := []tableColumn{
		{
			def:  tableColumnDefinition{Name: "Name", Type: "string", Format: "name"},
			path: ".metadata.name",
		},
	}
	for _, c := range fetchv1alpha1.NewCRD().Spec.Versions[0].AdditionalPrinterColumns {
		cols = append(cols, tableColumn{
			def: tableColumnDefinition{
				Name:        c.Name,
				Type:        c.Type,
				Format:      c.Format,
				Description: c.Description,
				Priority:    c.Priority,
			},
			path: c.JSONPath,
		})
	}

	defs := make([]tableColumnDefinition, 0, len(cols))
	for _, c := range cols {
		defs = append(defs, c.def)
	}

	t := &table{
		Kind:              "Table",
		APIVersion:        "meta.k8s.io/v1",
		Metadata:          metav1.ListMeta{ResourceVersion: strconv.FormatInt(resourceVersion, 10)},
		ColumnDefinitions: defs,
		Rows:              make([]tableRow, 0, len(items)),
	}

	for _, item := range items {
		normalize(item)
		raw, err := json.Marshal(item)
		if err != nil {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal(raw, &obj); err != nil {
			continue
		}

		cells := make([]any, 0, len(cols))
		for _, c := range cols {
			cells = append(cells, formatCell(valueAtPath(obj, c.path), c.def.Type))
		}
		t.Rows = append(t.Rows, tableRow{Cells: cells, Object: raw})
	}
	return t
}

func valueAtPath(obj map[string]any, path string) any {
	path = strings.TrimPrefix(path, "$")
	path = strings.TrimPrefix(path, ".")
	if path == "" {
		return nil
	}
	var cur any = obj
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur, ok = m[part]
		if !ok {
			return nil
		}
	}
	return cur
}

func formatCell(v any, typ string) any {
	if v == nil {
		return ""
	}
	switch typ {
	case "integer", "number":
		switch n := v.(type) {
		case float64:
			return int64(n)
		case int64:
			return n
		case int:
			return int64(n)
		}
		return fmt.Sprint(v)
	case "date":
		// metav1.Time serialises as an RFC3339 string; keep it as-is.
		return fmt.Sprint(v)
	default:
		return fmt.Sprint(v)
	}
}
