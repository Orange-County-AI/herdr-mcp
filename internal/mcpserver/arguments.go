package mcpserver

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

var argumentAliases = map[string]map[string]string{
	"worktree.create": {"repo": "cwd", "repo_path": "cwd"},
	"agent.prompt":    {"prompt": "text", "message": "text", "body": "text"},
	"agent.start":     {"agent": "kind", "harness": "kind"},
}

func normalizeArguments(method string, input map[string]any, raw json.RawMessage) (json.RawMessage, []string, error) {
	arguments := map[string]any{}
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &arguments); err != nil {
			return nil, nil, fmt.Errorf("arguments must be a JSON object: %w", err)
		}
	}

	aliases := argumentAliases[method]
	notes := make([]string, 0, len(aliases))
	for alias, canonical := range aliases {
		value, present := arguments[alias]
		if !present {
			continue
		}
		if _, alreadySet := arguments[canonical]; alreadySet {
			return nil, nil, fmt.Errorf("arguments %q and its alias %q cannot both be set", canonical, alias)
		}
		arguments[canonical] = value
		delete(arguments, alias)
		notes = append(notes, fmt.Sprintf("Mapped argument %q to %q.", alias, canonical))
	}

	if err := validateArgumentObject(input, arguments, input); err != nil {
		return nil, nil, fmt.Errorf("invalid arguments for %s: %w", strings.ReplaceAll(method, ".", "_"), err)
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return nil, nil, fmt.Errorf("encode normalized arguments: %w", err)
	}
	return encoded, notes, nil
}

func validateArgumentObject(schema map[string]any, arguments map[string]any, root map[string]any) error {
	properties, _ := schema["properties"].(map[string]any)
	accepted := make([]string, 0, len(properties))
	for name := range properties {
		accepted = append(accepted, name)
	}
	sort.Strings(accepted)
	for name := range arguments {
		if _, known := properties[name]; !known {
			message := fmt.Sprintf("%q is not accepted (accepted: %s)", name, strings.Join(accepted, ", "))
			if suggestion := closestParameter(name, accepted); suggestion != "" {
				message += fmt.Sprintf("; did you mean %q?", suggestion)
			}
			return fmt.Errorf("%s", message)
		}
	}
	if required, ok := schema["required"].([]any); ok {
		for _, value := range required {
			name, _ := value.(string)
			if _, present := arguments[name]; !present {
				return fmt.Errorf("missing required %q (accepted: %s)", name, strings.Join(accepted, ", "))
			}
		}
	}
	for name, value := range arguments {
		property, _ := properties[name].(map[string]any)
		if err := validateArgumentValue(name, property, value, root); err != nil {
			return err
		}
	}
	return nil
}

func validateArgumentValue(name string, schema map[string]any, value any, root map[string]any) error {
	resolved, err := resolveSchema(schema, root)
	if err != nil {
		return err
	}
	if enum, ok := resolved["enum"].([]any); ok {
		for _, allowed := range enum {
			if value == allowed {
				return nil
			}
		}
		values := make([]string, 0, len(enum))
		for _, allowed := range enum {
			values = append(values, fmt.Sprintf("%q", allowed))
		}
		return fmt.Errorf("%q must be one of %s", name, strings.Join(values, ", "))
	}
	return nil
}

func resolveSchema(schema map[string]any, root map[string]any) (map[string]any, error) {
	reference, ok := schema["$ref"].(string)
	if !ok {
		return schema, nil
	}
	const prefix = "#/$defs/"
	if !strings.HasPrefix(reference, prefix) {
		return nil, fmt.Errorf("unsupported schema reference %q", reference)
	}
	definitions, _ := root["$defs"].(map[string]any)
	resolved, ok := definitions[strings.TrimPrefix(reference, prefix)].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("missing schema definition %q", reference)
	}
	return resolved, nil
}

func closestParameter(input string, candidates []string) string {
	best := ""
	bestDistance := int(^uint(0) >> 1)
	for _, candidate := range candidates {
		distance := levenshtein(input, candidate)
		if distance < bestDistance {
			best, bestDistance = candidate, distance
		}
	}
	if bestDistance > max(2, len(input)/2) {
		return ""
	}
	return best
}

func levenshtein(left, right string) int {
	previous := make([]int, len(right)+1)
	current := make([]int, len(right)+1)
	for index := range previous {
		previous[index] = index
	}
	for leftIndex, leftRune := range left {
		current[0] = leftIndex + 1
		for rightIndex, rightRune := range right {
			cost := 0
			if leftRune != rightRune {
				cost = 1
			}
			current[rightIndex+1] = min(
				current[rightIndex]+1,
				previous[rightIndex+1]+1,
				previous[rightIndex]+cost,
			)
		}
		previous, current = current, previous
	}
	return previous[len(right)]
}
