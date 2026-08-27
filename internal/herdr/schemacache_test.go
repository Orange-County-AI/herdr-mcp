package herdr

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const cacheableSchema = `{
  "protocol": 20,
  "schema_version": 1,
  "schemas": {
    "request": {
      "oneOf": [{
        "properties": {
          "method": {"const": "pane.read"},
          "params": {"$ref": "#/schemas/request/$defs/PaneReadParams"}
        }
      }],
      "$defs": {
        "PaneReadParams": {"type": "object", "properties": {"pane_id": {"type": "string"}}}
      }
    }
  }
}`

// A Herdr upgrade replaces the binary and stops the session at the same time.
// If the bridge could only get its schema from that binary it would be unable
// to start during exactly the outage it is supposed to cover.
func TestLoadSchemaCachedFallsBackWhenTheBinaryIsGone(t *testing.T) {
	cache := &SchemaCache{Path: filepath.Join(t.TempDir(), "schema.json")}
	if err := cache.Save([]byte(cacheableSchema)); err != nil {
		t.Fatal(err)
	}

	schema, note, err := LoadSchemaCached(context.Background(), "herdr-binary-that-does-not-exist", cache)
	if err != nil {
		t.Fatalf("cached fallback failed: %v", err)
	}
	if schema.Protocol != 20 {
		t.Fatalf("protocol = %d, want 20", schema.Protocol)
	}
	if !strings.Contains(note, "cached Herdr schema") {
		t.Fatalf("fallback was silent; note = %q", note)
	}
}

// With neither a binary nor a cache there is no honest tool surface to serve,
// and the error must name both failures so the operator knows which to fix.
func TestLoadSchemaCachedFailsWithoutBinaryOrCache(t *testing.T) {
	cache := &SchemaCache{Path: filepath.Join(t.TempDir(), "schema.json")}
	_, _, err := LoadSchemaCached(context.Background(), "herdr-binary-that-does-not-exist", cache)
	if err == nil {
		t.Fatal("startup succeeded with no schema source at all")
	}
	if !strings.Contains(err.Error(), "cached schema unusable") {
		t.Fatalf("error does not mention the cache: %v", err)
	}
}

func TestSchemaCacheSaveIsReadableOnlyByTheOwner(t *testing.T) {
	cache := &SchemaCache{Path: filepath.Join(t.TempDir(), "nested", "schema.json")}
	if err := cache.Save([]byte(cacheableSchema)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(cache.Path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("schema cache mode = %o, want 600", mode)
	}
	if _, err := cache.Load(); err != nil {
		t.Fatalf("saved schema did not load back: %v", err)
	}
}
