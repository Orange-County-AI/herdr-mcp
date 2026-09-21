package herdr

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var testMachines = []Machine{
	{ID: "0fb972d6", Label: "minime", Target: "minime", Session: "default", Enabled: true},
	{ID: "439a9698", Label: "gigachad", Target: "gigachad", Session: "default", Enabled: true},
	{ID: "aaa11122", Label: "MiniMe", Target: "other", Session: "work", Enabled: true},
	{ID: "bbb22233", Label: "retired", Target: "retired", Session: "default", Enabled: false},
}

func TestSelectMachineByLabelAndID(t *testing.T) {
	for _, selector := range []string{"minime", "0fb972d6"} {
		machine, err := SelectMachine(testMachines, selector)
		if err != nil {
			t.Fatalf("SelectMachine(%q): %v", selector, err)
		}
		if machine.Target != "minime" {
			t.Fatalf("SelectMachine(%q).Target = %q, want minime", selector, machine.Target)
		}
	}
}

// Label matching must stay case-sensitive. Two profiles differing only in case
// are legal, and folding case would silently send a mutation to whichever one
// happened to be first in the list.
func TestSelectMachineIsCaseSensitive(t *testing.T) {
	machine, err := SelectMachine(testMachines, "MiniMe")
	if err != nil {
		t.Fatal(err)
	}
	if machine.Target != "other" {
		t.Fatalf("Target = %q, want other", machine.Target)
	}
	if _, err := SelectMachine(testMachines, "MINIME"); err == nil {
		t.Fatal("SelectMachine(MINIME) succeeded; case folding would pick the wrong host")
	}
}

func TestSelectMachineRejectsDisabledAndUnknown(t *testing.T) {
	_, err := SelectMachine(testMachines, "retired")
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("disabled machine error = %v", err)
	}
	_, err = SelectMachine(testMachines, "nowhere")
	if err == nil || !strings.Contains(err.Error(), "minime") {
		t.Fatalf("unknown machine error = %v; want it to list the known labels", err)
	}
}

func TestSessionNameTreatsDefaultAsUnset(t *testing.T) {
	if got := (Machine{Session: "default"}).SessionName(); got != "" {
		t.Fatalf("SessionName(default) = %q, want empty", got)
	}
	if got := (Machine{Session: "work"}).SessionName(); got != "work" {
		t.Fatalf("SessionName(work) = %q", got)
	}
}

func TestListMachinesParsesBinaryOutput(t *testing.T) {
	binary := fakeHerdr(t, `[
	  {"id":"439a9698","label":"gigachad","target":"gigachad","session":"default","enabled":true,"selected":false},
	  {"id":"0fb972d6","label":"minime","target":"minime","session":"default","enabled":true,"selected":true}
	]`)
	machines, err := ListMachines(context.Background(), binary)
	if err != nil {
		t.Fatal(err)
	}
	if len(machines) != 2 {
		t.Fatalf("machines = %d, want 2", len(machines))
	}
	// Sorted by label, so a caller reading the list gets a stable order.
	if machines[0].Label != "gigachad" || machines[1].Label != "minime" {
		t.Fatalf("labels = %q, %q", machines[0].Label, machines[1].Label)
	}
}

func TestListMachinesReportsBinaryFailure(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "herdr")
	script := "#!/bin/sh\necho 'unknown option: --json' >&2\nexit 2\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := ListMachines(context.Background(), binary); err == nil || !strings.Contains(err.Error(), "unknown option") {
		t.Fatalf("err = %v; want the binary's own stderr", err)
	}
}

func TestSocketTagIsUniquePerProfile(t *testing.T) {
	// Two labels that sanitize identically must not share a socket path, or one
	// machine would be driven through the other's tunnel.
	first := socketTag(Machine{ID: "0fb972d6907b", Label: "web.one"})
	second := socketTag(Machine{ID: "439a9698c799", Label: "web/one"})
	if first == second {
		t.Fatalf("socketTag collision: %q", first)
	}
	if len(first) > 32 {
		t.Fatalf("socketTag %q is too long for a unix socket path", first)
	}
}

// fakeHerdr writes a stub binary that prints payload for `machine list --json`.
func fakeHerdr(t *testing.T, payload string) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "herdr")
	script := "#!/bin/sh\ncat <<'JSON'\n" + payload + "\nJSON\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return binary
}
