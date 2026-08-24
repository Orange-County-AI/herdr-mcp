package herdr

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path"
	"strings"
)

const requestDefinitionPrefix = "#/schemas/request/$defs/"

// Schema is the protocol metadata emitted by `herdr api schema --json`.
type Schema struct {
	Protocol      int                        `json:"protocol"`
	SchemaVersion int                        `json:"schema_version"`
	Schemas       map[string]json.RawMessage `json:"schemas"`
}

// MethodDefinition describes one non-streaming Herdr socket method as an MCP tool.
type MethodDefinition struct {
	Method      string
	ToolName    string
	InputSchema map[string]any
}

// LoadSchema asks the selected Herdr binary for the protocol schema it was built with.
func LoadSchema(ctx context.Context, binary string) (*Schema, error) {
	cmd := exec.CommandContext(ctx, binary, "api", "schema", "--json")
	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("%s api schema --json: %w: %s", binary, err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return nil, fmt.Errorf("%s api schema --json: %w", binary, err)
	}
	return ParseSchema(output)
}

// ParseSchema parses Herdr's versioned API schema document.
func ParseSchema(data []byte) (*Schema, error) {
	var schema Schema
	if err := json.Unmarshal(data, &schema); err != nil {
		return nil, fmt.Errorf("decode Herdr API schema: %w", err)
	}
	if schema.Protocol <= 0 {
		return nil, fmt.Errorf("Herdr API schema has invalid protocol %d", schema.Protocol)
	}
	if schema.SchemaVersion <= 0 {
		return nil, fmt.Errorf("Herdr API schema has invalid schema_version %d", schema.SchemaVersion)
	}
	if len(schema.Schemas["request"]) == 0 {
		return nil, fmt.Errorf("Herdr API schema does not contain schemas.request")
	}
	return &schema, nil
}

// Methods turns request variants into compact, standalone MCP input schemas.
// Each returned schema carries only the transitive $defs used by that method.
func (s *Schema) Methods(allow, deny []string) ([]MethodDefinition, error) {
	if err := validatePatterns(append(append([]string(nil), allow...), deny...)); err != nil {
		return nil, err
	}

	var request struct {
		OneOf []map[string]any `json:"oneOf"`
		Defs  map[string]any   `json:"$defs"`
	}
	if err := json.Unmarshal(s.Schemas["request"], &request); err != nil {
		return nil, fmt.Errorf("decode schemas.request: %w", err)
	}
	if len(request.OneOf) == 0 || len(request.Defs) == 0 {
		return nil, fmt.Errorf("schemas.request is missing oneOf variants or $defs")
	}

	methods := make([]MethodDefinition, 0, len(request.OneOf))
	seen := make(map[string]struct{}, len(request.OneOf))
	for _, variant := range request.OneOf {
		method, ref, err := methodVariant(variant)
		if err != nil {
			return nil, err
		}
		if !matchesFilter(method, allow, deny) {
			continue
		}
		if _, exists := seen[method]; exists {
			return nil, fmt.Errorf("schemas.request contains duplicate method %q", method)
		}
		seen[method] = struct{}{}

		rootName := strings.TrimPrefix(ref, requestDefinitionPrefix)
		root, ok := request.Defs[rootName]
		if !ok {
			return nil, fmt.Errorf("method %q references missing definition %q", method, rootName)
		}
		input, err := cloneObject(root)
		if err != nil {
			return nil, fmt.Errorf("method %q input schema: %w", method, err)
		}
		if input["type"] != "object" {
			return nil, fmt.Errorf("method %q input schema must be an object", method)
		}

		closure := make(map[string]any)
		for _, reference := range requestReferences(input) {
			if err := collectDefinitions(strings.TrimPrefix(reference, requestDefinitionPrefix), request.Defs, closure); err != nil {
				return nil, fmt.Errorf("method %q input schema: %w", method, err)
			}
		}
		standaloneDefs := make(map[string]any, len(closure))
		for name, definition := range closure {
			copy, err := cloneValue(definition)
			if err != nil {
				return nil, fmt.Errorf("method %q definition %q: %w", method, name, err)
			}
			rewriteRequestRefs(copy)
			standaloneDefs[name] = copy
		}
		rewriteRequestRefs(input)
		inlineEnumReferences(input, standaloneDefs)
		standaloneDefs = referencedDefinitions(input, standaloneDefs)
		if len(standaloneDefs) > 0 {
			input["$defs"] = standaloneDefs
		}

		methods = append(methods, MethodDefinition{
			Method:      method,
			ToolName:    strings.NewReplacer(".", "_", "-", "_").Replace(method),
			InputSchema: input,
		})
	}
	if len(methods) == 0 {
		return nil, fmt.Errorf("method filters expose no Herdr API methods")
	}
	return methods, nil
}

func methodVariant(variant map[string]any) (method, ref string, err error) {
	properties, ok := variant["properties"].(map[string]any)
	if !ok {
		return "", "", fmt.Errorf("schemas.request variant is missing properties")
	}
	methodSchema, ok := properties["method"].(map[string]any)
	if !ok {
		return "", "", fmt.Errorf("schemas.request variant is missing method schema")
	}
	method, ok = methodSchema["const"].(string)
	if !ok || method == "" {
		return "", "", fmt.Errorf("schemas.request variant has no method const")
	}
	paramsSchema, ok := properties["params"].(map[string]any)
	if !ok {
		return "", "", fmt.Errorf("method %q is missing params schema", method)
	}
	ref, ok = paramsSchema["$ref"].(string)
	if !ok || !strings.HasPrefix(ref, requestDefinitionPrefix) {
		return "", "", fmt.Errorf("method %q has unsupported params schema reference %q", method, ref)
	}
	return method, ref, nil
}

func collectDefinitions(name string, definitions map[string]any, collected map[string]any) error {
	if _, ok := collected[name]; ok {
		return nil
	}
	definition, ok := definitions[name]
	if !ok {
		return fmt.Errorf("missing referenced definition %q", name)
	}
	collected[name] = definition
	for _, ref := range requestReferences(definition) {
		if err := collectDefinitions(strings.TrimPrefix(ref, requestDefinitionPrefix), definitions, collected); err != nil {
			return err
		}
	}
	return nil
}

func requestReferences(value any) []string {
	var refs []string
	var visit func(any)
	visit = func(current any) {
		switch typed := current.(type) {
		case map[string]any:
			for key, child := range typed {
				if key == "$ref" {
					if ref, ok := child.(string); ok && strings.HasPrefix(ref, requestDefinitionPrefix) {
						refs = append(refs, ref)
					}
				}
				visit(child)
			}
		case []any:
			for _, child := range typed {
				visit(child)
			}
		}
	}
	visit(value)
	return refs
}

func rewriteRequestRefs(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == "$ref" {
				if ref, ok := child.(string); ok && strings.HasPrefix(ref, requestDefinitionPrefix) {
					typed[key] = "#/$defs/" + strings.TrimPrefix(ref, requestDefinitionPrefix)
					continue
				}
			}
			rewriteRequestRefs(child)
		}
	case []any:
		for _, child := range typed {
			rewriteRequestRefs(child)
		}
	}
}

// inlineEnumReferences replaces standalone enum definitions with the enum at
// its use site. This keeps client-facing schemas compact while preserving every
// permitted value at the parameter that needs it.
func inlineEnumReferences(value any, definitions map[string]any) {
	switch typed := value.(type) {
	case map[string]any:
		if reference, ok := typed["$ref"].(string); ok && len(typed) == 1 && strings.HasPrefix(reference, "#/$defs/") {
			name := strings.TrimPrefix(reference, "#/$defs/")
			if definition, ok := definitions[name].(map[string]any); ok && definition["enum"] != nil {
				for key := range typed {
					delete(typed, key)
				}
				for key, child := range definition {
					typed[key] = child
				}
				return
			}
		}
		for _, child := range typed {
			inlineEnumReferences(child, definitions)
		}
	case []any:
		for _, child := range typed {
			inlineEnumReferences(child, definitions)
		}
	}
}

// referencedDefinitions retains only definitions reachable from the input
// schema after leaf enums have been inlined.
func referencedDefinitions(input map[string]any, definitions map[string]any) map[string]any {
	used := make(map[string]struct{})
	var addReferences func(any)
	addReferences = func(value any) {
		switch typed := value.(type) {
		case map[string]any:
			if reference, ok := typed["$ref"].(string); ok && strings.HasPrefix(reference, "#/$defs/") {
				name := strings.TrimPrefix(reference, "#/$defs/")
				if _, seen := used[name]; !seen {
					used[name] = struct{}{}
					addReferences(definitions[name])
				}
			}
			for _, child := range typed {
				addReferences(child)
			}
		case []any:
			for _, child := range typed {
				addReferences(child)
			}
		}
	}
	addReferences(input)

	retained := make(map[string]any, len(used))
	for name := range used {
		retained[name] = definitions[name]
	}
	return retained
}

func cloneObject(value any) (map[string]any, error) {
	copy, err := cloneValue(value)
	if err != nil {
		return nil, err
	}
	object, ok := copy.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("expected JSON object")
	}
	return object, nil
}

func cloneValue(value any) (any, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var copy any
	if err := json.Unmarshal(data, &copy); err != nil {
		return nil, err
	}
	return copy, nil
}

func validatePatterns(patterns []string) error {
	for _, pattern := range patterns {
		if _, err := path.Match(pattern, ""); err != nil {
			return fmt.Errorf("invalid method pattern %q: %w", pattern, err)
		}
	}
	return nil
}

func matchesFilter(method string, allow, deny []string) bool {
	if len(allow) > 0 && !matchesAny(method, allow) {
		return false
	}
	return !matchesAny(method, deny)
}

func matchesAny(method string, patterns []string) bool {
	for _, pattern := range patterns {
		if matched, _ := path.Match(pattern, method); matched {
			return true
		}
	}
	return false
}
