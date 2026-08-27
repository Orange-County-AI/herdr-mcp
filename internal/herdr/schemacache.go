package herdr

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// SchemaCache keeps the last schema a Herdr binary produced, so the bridge can
// register its tools when that binary is missing, mid-upgrade, or failing.
//
// The schema comes from `herdr api schema --json`, which needs the binary but
// not a running session -- yet the binary is unavailable during exactly the
// upgrades that also take the session down. Without a cache the bridge cannot
// start at all in that window, which is the outage it exists to survive.
type SchemaCache struct {
	Path string
}

// DefaultSchemaCache resolves the per-user cache location.
func DefaultSchemaCache() (*SchemaCache, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return nil, fmt.Errorf("resolve user cache directory: %w", err)
	}
	return &SchemaCache{Path: filepath.Join(dir, "herdr-mcp", "schema.json")}, nil
}

// Load reads the cached schema document.
func (c *SchemaCache) Load() (*Schema, error) {
	if c == nil || c.Path == "" {
		return nil, fmt.Errorf("no schema cache configured")
	}
	data, err := os.ReadFile(c.Path)
	if err != nil {
		return nil, fmt.Errorf("read cached schema %s: %w", c.Path, err)
	}
	return ParseSchema(data)
}

// Save stores a schema document that already parsed successfully. A failure to
// write is reported but never fatal: the bridge is running, and losing the
// cache only costs it the next cold start.
func (c *SchemaCache) Save(data []byte) error {
	if c == nil || c.Path == "" {
		return fmt.Errorf("no schema cache configured")
	}
	if err := os.MkdirAll(filepath.Dir(c.Path), 0o700); err != nil {
		return fmt.Errorf("create schema cache directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(c.Path), ".schema-*")
	if err != nil {
		return fmt.Errorf("create temporary schema cache: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write schema cache: %w", err)
	}
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("set schema cache mode: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close schema cache: %w", err)
	}
	if err := os.Rename(temporaryPath, c.Path); err != nil {
		return fmt.Errorf("install schema cache: %w", err)
	}
	return nil
}

// LoadSchemaCached asks the Herdr binary for the schema and refreshes the
// cache, falling back to the cached copy when the binary cannot answer. The
// returned note is non-empty when the fallback was used and belongs in the
// startup log: serving stale tools silently is worse than serving them loudly.
func LoadSchemaCached(ctx context.Context, binary string, cache *SchemaCache) (*Schema, string, error) {
	data, liveErr := loadSchemaBytes(ctx, binary)
	if liveErr == nil {
		schema, parseErr := ParseSchema(data)
		if parseErr == nil {
			if err := cache.Save(data); err != nil {
				return schema, fmt.Sprintf("schema cache not updated: %v", err), nil
			}
			return schema, "", nil
		}
		liveErr = parseErr
	}
	schema, cacheErr := cache.Load()
	if cacheErr != nil {
		return nil, "", fmt.Errorf("%w (cached schema unusable: %v)", liveErr, cacheErr)
	}
	return schema, fmt.Sprintf("using cached Herdr schema (protocol %d) because the binary could not supply one: %v", schema.Protocol, liveErr), nil
}
