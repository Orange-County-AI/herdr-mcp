package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeArgumentsAcceptsDocumentedAliases(t *testing.T) {
	input := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"target": map[string]any{"type": "string"},
			"text":   map[string]any{"type": "string"},
		},
		"required": []any{"target", "text"},
	}
	normalized, notes, err := normalizeArguments("agent.prompt", input, json.RawMessage(`{"target":"reviewer","prompt":"inspect this"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], `"prompt"`) {
		t.Fatalf("notes = %q", notes)
	}
	if string(normalized) != `{"target":"reviewer","text":"inspect this"}` {
		t.Fatalf("normalized = %s", normalized)
	}
}

func TestNormalizeArgumentsRejectsUnknownAndMissingValues(t *testing.T) {
	input := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"target": map[string]any{"type": "string"},
			"source": map[string]any{"enum": []any{"visible", "recent"}, "type": "string"},
		},
		"required": []any{"target", "source"},
	}
	for _, test := range []struct {
		name string
		args string
		want string
	}{
		{"unknown", `{"target":"p1","soruce":"recent"}`, `did you mean "source"?`},
		{"missing", `{"target":"p1"}`, `missing required "source"`},
		{"enum", `{"target":"p1","source":"screen"}`, `"source" must be one of "visible", "recent"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := normalizeArguments("pane.read", input, json.RawMessage(test.args))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err = %v, want %q", err, test.want)
			}
		})
	}
}

func TestToolDescriptionsUseClientVocabulary(t *testing.T) {
	for method, terms := range map[string][]string{
		"agent.list":   {"running coding agents", "status"},
		"agent.read":   {"output", "recent_unwrapped"},
		"agent.prompt": {"task prompt", "text"},
		"agent.start":  {"existing available shell pane", "does not create"},
	} {
		description := toolDescription(method, map[string]any{"type": "object"})
		for _, term := range terms {
			if !strings.Contains(description, term) {
				t.Errorf("%s description missing %q: %s", method, term, description)
			}
		}
	}
}
