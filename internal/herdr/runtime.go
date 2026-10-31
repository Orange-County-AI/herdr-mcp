package herdr

import (
	"fmt"
	"os"
	"syscall"
)

// ensureRuntimeDir restores a removed runtime directory without accepting an
// existing symlink, shared directory, or directory owned by another user.
// Never chmod/chown an existing path: an unsafe replacement must fail closed.
func ensureRuntimeDir(dir string) error {
	if dir == "" {
		return fmt.Errorf("runtime directory must not be empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create runtime directory %s: %w", dir, err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect runtime directory %s: %w", dir, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("runtime directory %s is not a real directory", dir)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("runtime directory %s is not owned by the current user", dir)
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("runtime directory %s must have mode 0700 (has %04o)", dir, info.Mode().Perm())
	}
	return nil
}
