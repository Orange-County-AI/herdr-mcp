package herdr

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeDirRestoresMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runtime")
	for i := 0; i < 2; i++ {
		if err := ensureRuntimeDir(dir); err != nil {
			t.Fatal(err)
		}
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatalf("runtime directory not private: info=%v err=%v", info, err)
		}
		if err := os.Remove(dir); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRuntimeDirRejectsUnsafeExistingPaths(t *testing.T) {
	for _, kind := range []string{"symlink", "file", "shared", "nonsearchable"} {
		t.Run(kind, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "runtime")
			switch kind {
			case "symlink":
				if err := os.Symlink(t.TempDir(), dir); err != nil {
					t.Fatal(err)
				}
			case "file":
				if err := os.WriteFile(dir, []byte("keep"), 0o600); err != nil {
					t.Fatal(err)
				}
			default:
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				mode := os.FileMode(0o755)
				if kind == "nonsearchable" {
					mode = 0o600
				}
				if err := os.Chmod(dir, mode); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.Lstat(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := ensureRuntimeDir(dir); err == nil {
				t.Fatal("accepted unsafe runtime path")
			}
			after, err := os.Lstat(dir)
			if err != nil || before.Mode() != after.Mode() {
				t.Fatal("changed unsafe path")
			}
		})
	}
}

func TestDefaultRuntimeDirRestoresRemovedDirectory(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	dir, err := DefaultRuntimeDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	restored, err := DefaultRuntimeDir()
	if err != nil || restored != dir {
		t.Fatalf("restored=%q err=%v", restored, err)
	}
}

func TestDialRemoteRecreatesRuntimeDirBeforeForward(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "runtime")
	if err := ensureRuntimeDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	// No real host or SSH connection: emulate a successful status probe, then
	// record whether DialRemote restored the directory before forwarding.
	bin := filepath.Join(base, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(base, "forward-ok")
	script := "#!/bin/sh\ncase \" $* \" in\n*' -fNT '*)\n [ -d \"$TEST_RUNTIME_DIR\" ] || exit 43\n touch \"$TEST_FORWARD_MARKER\"\n echo test-forward-stopped >&2\n exit 42;;\n*' -O exit '*) exit 0;;\n*) echo '{\"running\":true,\"version\":\"0.9.5\",\"protocol\":22,\"socket\":\"/tmp/test-herdr.sock\"}';;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TEST_RUNTIME_DIR", dir)
	t.Setenv("TEST_FORWARD_MARKER", marker)
	_, err := DialRemote(context.Background(), Machine{ID: "test", Label: "test", Target: "fake", Enabled: true}, 22, dir)
	if err == nil || !strings.Contains(err.Error(), "test-forward-stopped") {
		t.Fatalf("unexpected dial result: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("forward did not see restored directory: %v", err)
	}
}

func TestRemoteRetirementProtectsActiveCalls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remote.sock")
	entered, release := make(chan struct{}), make(chan struct{})
	stop := serveSocket(t, path, nil, func(request map[string]any) string {
		close(entered)
		<-release
		return `{"ok":true}`
	})
	defer stop()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	remote := &Remote{Queue: NewQueue(ctx, &Client{SocketPath: path}, 22)}
	done := make(chan error, 1)
	go func() { _, err := remote.Call(ctx, "pane.send_text", nil); done <- err }()
	<-entered
	if remote.retireIfIdle() {
		t.Fatal("retired an in-flight call")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("active call interrupted: %v", err)
	}
	if !remote.retireIfIdle() {
		t.Fatal("idle remote could not retire")
	}
	if _, err := remote.Call(ctx, "pane.send_text", nil); err == nil || !strings.Contains(err.Error(), "before dispatch") {
		t.Fatalf("old reference did not fail before dispatch: %v", err)
	}
}
