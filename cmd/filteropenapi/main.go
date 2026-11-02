// Command filteropenapi builds the OpenAPI 3.0 spec for oapi-codegen (openapi.codegen.json) from
// the API's full spec.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var keepPaths = []string{
	"/v1/ingest/server-events",
	"/v1/seeds/server-resolve",
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintf(os.Stderr, "usage: filteropenapi <input.json> <output.json>\n")
		os.Exit(1)
	}
	inPath, _ := filepath.Abs(os.Args[1])
	outPath, _ := filepath.Abs(os.Args[2])

	raw, err := os.ReadFile(filepath.Clean(inPath)) //nolint:gosec // codegen input path from go:generate
	if err != nil {
		fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		fatal(err)
	}

	paths, _ := doc["paths"].(map[string]any)
	if paths == nil {
		fatal(fmt.Errorf("missing paths"))
	}
	filteredPaths := make(map[string]any, len(keepPaths))
	for _, p := range keepPaths {
		if v, ok := paths[p]; ok {
			filteredPaths[p] = v
		}
	}

	components, _ := doc["components"].(map[string]any)
	schemas, _ := components["schemas"].(map[string]any)
	if schemas == nil {
		fatal(fmt.Errorf("missing components.schemas"))
	}

	need := map[string]struct{}{}
	var pending []string
	addRef := func(name string) {
		if name == "" {
			return
		}
		if _, ok := need[name]; ok {
			return
		}
		need[name] = struct{}{}
		pending = append(pending, name)
	}
	collectRefs(filteredPaths, addRef)
	for len(pending) > 0 {
		name := pending[0]
		pending = pending[1:]
		if s, ok := schemas[name]; ok {
			collectRefs(s, addRef)
		}
	}

	outSchemas := make(map[string]any, len(need))
	for name := range need {
		if s, ok := schemas[name]; ok {
			outSchemas[name] = downgradeSchema(s)
		}
	}

	out := map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":   "kalathos SDK API subset",
			"version": "1.0.0",
		},
		"paths": filteredPaths,
		"components": map[string]any{
			"schemas": outSchemas,
		},
	}
	outBytes, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		fatal(err)
	}
	if err := os.WriteFile(filepath.Clean(outPath), append(outBytes, '\n'), 0o600); err != nil { //nolint:gosec // generated artifact beside module
		fatal(err)
	}
}

func collectRefs(v any, addRef func(string)) {
	switch x := v.(type) {
	case map[string]any:
		if ref, ok := x["$ref"].(string); ok {
			if name, ok := strings.CutPrefix(ref, "#/components/schemas/"); ok {
				addRef(name)
			}
		}
		for _, child := range x {
			collectRefs(child, addRef)
		}
	case []any:
		for _, child := range x {
			collectRefs(child, addRef)
		}
	}
}

func downgradeSchema(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = downgradeSchema(val)
		}
		if types, ok := out["type"].([]any); ok {
			nullable := false
			var primary string
			for _, t := range types {
				s, _ := t.(string)
				if s == "null" {
					nullable = true
					continue
				}
				if primary == "" {
					primary = s
				}
			}
			if primary != "" {
				out["type"] = primary
			} else {
				delete(out, "type")
			}
			if nullable {
				out["nullable"] = true
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = downgradeSchema(val)
		}
		return out
	default:
		return v
	}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "filteropenapi: %v\n", err)
	os.Exit(1)
}
