package herdr

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// Machine is one saved SSH machine profile, as `herdr machine list --json`
// reports it. Herdr 0.9 made these first-class: each one is an independent
// Herdr server with its own workspace, tab, pane and agent IDs, reached over
// SSH. Nothing about them appears in the socket protocol, so a profile is the
// only thing that maps a caller's "minime" to a host this bridge can dial.
type Machine struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Target  string `json:"target"`
	Session string `json:"session"`
	Enabled bool   `json:"enabled"`
}

// SessionName is the remote session a profile uses, or "" for Herdr's default.
func (m Machine) SessionName() string {
	if m.Session == "" || m.Session == "default" {
		return ""
	}
	return m.Session
}

// ListMachines reads the saved SSH machine profiles from the Herdr binary.
//
// This shells out rather than using the socket because machine profiles are
// client-side configuration: there is no machine.* method, and no field in the
// request envelope that routes a call anywhere but the socket it arrived on.
func ListMachines(ctx context.Context, binary string) ([]Machine, error) {
	if binary == "" {
		binary = "herdr"
	}
	cmd := exec.CommandContext(ctx, binary, "machine", "list", "--json")
	output, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			detail := strings.TrimSpace(string(exitErr.Stderr))
			if detail == "" {
				detail = err.Error()
			}
			return nil, fmt.Errorf("%s machine list --json: %s", binary, detail)
		}
		return nil, fmt.Errorf("%s machine list --json: %w", binary, err)
	}
	var machines []Machine
	if err := json.Unmarshal(output, &machines); err != nil {
		return nil, fmt.Errorf("decode machine list: %w", err)
	}
	sort.Slice(machines, func(i, j int) bool { return machines[i].Label < machines[j].Label })
	return machines, nil
}

// SelectMachine resolves a caller's selector the way Herdr's own --machine
// does: an enabled profile's ID, or its exact label. Label matching is
// case-sensitive deliberately -- two profiles may differ only in case, and
// silently picking one of them would send a mutation to the wrong host.
func SelectMachine(machines []Machine, selector string) (Machine, error) {
	if selector == "" {
		return Machine{}, fmt.Errorf("no machine selector given")
	}
	var disabled *Machine
	for index := range machines {
		machine := machines[index]
		if machine.ID != selector && machine.Label != selector {
			continue
		}
		if !machine.Enabled {
			disabled = &machines[index]
			continue
		}
		return machine, nil
	}
	if disabled != nil {
		return Machine{}, fmt.Errorf("machine %q is disabled; enable it with `herdr machine enable %s`", selector, disabled.Label)
	}
	return Machine{}, fmt.Errorf("no saved Herdr machine matches %q (known: %s). Labels are case-sensitive; use machine_list to see them",
		selector, describeMachines(machines))
}

func describeMachines(machines []Machine) string {
	if len(machines) == 0 {
		return "none saved; add one with `herdr machine add <ssh-target>`"
	}
	labels := make([]string, 0, len(machines))
	for _, machine := range machines {
		if machine.Enabled {
			labels = append(labels, machine.Label)
		}
	}
	if len(labels) == 0 {
		return "none enabled"
	}
	return strings.Join(labels, ", ")
}
