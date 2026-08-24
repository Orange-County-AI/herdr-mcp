package herdr

import (
	"encoding/json"
	"testing"
)

const testSchema = `{
  "protocol": 20,
  "schema_version": 1,
  "schemas": {
    "request": {
      "oneOf": [
        {
          "type": "object",
          "properties": {
            "method": {"const": "ping", "type": "string"},
            "params": {"$ref": "#/schemas/request/$defs/EmptyParams"}
          }
        },
        {
          "type": "object",
          "properties": {
            "method": {"const": "pane.read", "type": "string"},
            "params": {"$ref": "#/schemas/request/$defs/PaneReadParams"}
          }
        },
        {
          "type": "object",
          "properties": {
            "method": {"const": "events.subscribe", "type": "string"},
            "params": {"$ref": "#/schemas/request/$defs/EmptyParams"}
          }
        }
      ],
      "$defs": {
        "EmptyParams": {"type": "object", "properties": {}},
        "PaneReadParams": {
          "type": "object",
          "properties": {
            "pane_id": {"type": "string"},
            "source": {"$ref": "#/schemas/request/$defs/ReadSource"}
          },
          "required": ["pane_id", "source"]
        },
        "ReadSource": {"type": "string", "enum": ["visible", "recent"]},
        "Unused": {"type": "string"}
      }
    }
  }
}`

func TestMethodsBuildStandaloneSchemas(t *testing.T) {
	schema, err := ParseSchema([]byte(testSchema))
	if err != nil {
		t.Fatal(err)
	}
	methods, err := schema.Methods(nil, []string{"events.*"})
	if err != nil {
		t.Fatal(err)
	}
	if len(methods) != 2 {
		t.Fatalf("methods = %d, want 2", len(methods))
	}
	if methods[1].Method != "pane.read" || methods[1].ToolName != "pane_read" {
		t.Fatalf("method = %+v", methods[1])
	}
	if _, ok := methods[1].InputSchema["$defs"]; ok {
		t.Fatalf("unneeded $defs were retained: %v", methods[1].InputSchema["$defs"])
	}
	properties := methods[1].InputSchema["properties"].(map[string]any)
	source := properties["source"].(map[string]any)
	if source["enum"] == nil {
		t.Fatalf("source enum was not inlined: %v", source)
	}
	if source["$ref"] != nil {
		t.Fatalf("source ref was retained: %v", source["$ref"])
	}

	encoded, err := json.Marshal(methods[1].InputSchema)
	if err != nil || !json.Valid(encoded) {
		t.Fatalf("standalone schema is not valid JSON: %v", err)
	}
}

func TestMethodsApplyAllowAndDeny(t *testing.T) {
	schema, err := ParseSchema([]byte(testSchema))
	if err != nil {
		t.Fatal(err)
	}
	methods, err := schema.Methods([]string{"pane.*", "events.*"}, []string{"events.subscribe"})
	if err != nil {
		t.Fatal(err)
	}
	if len(methods) != 1 || methods[0].Method != "pane.read" {
		t.Fatalf("methods = %+v", methods)
	}
	if _, err := schema.Methods([]string{"["}, nil); err == nil {
		t.Fatal("invalid glob was accepted")
	}
	if _, err := schema.Methods([]string{"agent.*"}, nil); err == nil {
		t.Fatal("empty method selection was accepted")
	}
}

func TestParseSchemaRequiresVersionedRequestSchema(t *testing.T) {
	for _, input := range []string{
		`{}`,
		`{"protocol":20,"schema_version":1,"schemas":{}}`,
		`{"protocol":0,"schema_version":1,"schemas":{"request":{}}}`,
	} {
		if _, err := ParseSchema([]byte(input)); err == nil {
			t.Fatalf("ParseSchema(%s) succeeded", input)
		}
	}
}
