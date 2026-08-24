package mcpserver

import (
	"fmt"
	"sort"
	"strings"
)

// toolDescription uses caller vocabulary rather than exposing the socket
// protocol as documentation. The fallback keeps newly added Herdr methods
// distinguishable until their dedicated description is added here.
func toolDescription(method string, input map[string]any) string {
	if description, ok := curatedToolDescriptions[method]; ok {
		return description + inputSummary(input)
	}

	parts := strings.Split(method, ".")
	resource := strings.Join(parts[:len(parts)-1], " ")
	verb := parts[len(parts)-1]
	var action string
	switch verb {
	case "list":
		action = "List " + resource + " records."
	case "get", "current", "info", "layout", "edges", "neighbor", "process_info", "explain":
		action = "Inspect " + resource + " state."
	case "create", "open", "install", "link", "enable", "focus", "move", "rename", "set", "apply", "swap", "split", "resize", "zoom":
		action = "Change " + resource + " state."
	case "read":
		action = "Read " + resource + " output or state."
	case "wait", "wait_for_output":
		action = "Wait for " + resource + " state or output."
	case "close", "remove", "unlink", "uninstall", "disable", "clear_agent_authority", "release_agent":
		action = "Remove or stop " + resource + " state."
	default:
		action = fmt.Sprintf("Operate Herdr %s.", strings.ReplaceAll(method, ".", " "))
	}
	return action + inputSummary(input)
}

func inputSummary(input map[string]any) string {
	properties, _ := input["properties"].(map[string]any)
	if len(properties) == 0 {
		return " Inputs: none."
	}
	required := make(map[string]struct{})
	if values, ok := input["required"].([]any); ok {
		for _, value := range values {
			if name, ok := value.(string); ok {
				required[name] = struct{}{}
			}
		}
	}
	names := make([]string, 0, len(properties))
	for name := range properties {
		if _, ok := required[name]; ok {
			names = append(names, name+" (required)")
		} else {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return " Inputs: " + strings.Join(names, ", ") + "."
}

var curatedToolDescriptions = map[string]string{
	"ping":                          "Check whether the active Herdr session socket is reachable and protocol-compatible.",
	"session.snapshot":              "Get one snapshot of all workspaces, tabs, panes, and recognized agents. Use this to discover stable IDs before changing terminal state.",
	"workspace.create":              "Create a terminal workspace with a root pane. The result contains workspace_id, tab_id, and root_pane.pane_id for follow-up calls.",
	"worktree.create":               "Create and open a Git worktree as a workspace. Set cwd to the source repository path; the result contains root_pane.pane_id for agent_start. repo and repo_path are accepted aliases for cwd.",
	"worktree.open":                 "Open an existing Git worktree as a workspace. Set cwd or path to locate it; the result contains root_pane.pane_id.",
	"agent.list":                    "List running coding agents and their live status (idle, working, blocked, done, unknown), name, kind, pane_id, workspace, and cwd. Use agent_read for output or agent_prompt to send work.",
	"agent.get":                     "Inspect one running agent by unique name or pane_id, including launch_pending and interactive_ready during startup.",
	"agent.read":                    "Read an agent's output or scrollback by agent name or pane_id. source must be visible, recent, recent_unwrapped, or detection; prefer recent_unwrapped for transcripts and logs.",
	"agent.explain":                 "Explain why Herdr did or did not recognize a terminal as an agent, including detection state and manifest details.",
	"agent.send_keys":               "Send logical key presses to a running agent by name or pane_id. Use for explicit interactive controls such as Escape or Ctrl+C, not normal task prompts.",
	"agent.start":                   "Start a supported coding-agent harness in an existing available shell pane. This does not create or split a pane: obtain pane_id from worktree_create, workspace_create, tab_create, or pane_split first. kind is the harness (for example claude, codex, omp); name is the unique agent name. agent and harness are accepted aliases for kind.",
	"agent.prompt":                  "Send a task prompt to a running, interactive agent by name or pane_id. text is the prompt body; prompt, message, and body are accepted aliases. Wait for agent_start to return interactive_ready before prompting.",
	"agent.wait":                    "Wait for a named running agent to reach idle, done, blocked, or another requested status. If the agent is still launching, waits until it becomes addressable before waiting for the requested status.",
	"pane.list":                     "List terminal panes with stable pane IDs, workspace, tab, working directory, and recognized agent state.",
	"pane.current":                  "Get the current terminal pane and its stable pane_id. Prefer explicit pane IDs rather than relying on UI focus for mutations.",
	"pane.split":                    "Split a terminal pane to create a new shell pane. The result contains the new pane_id; use it as agent_start.pane_id to launch an agent.",
	"pane.read":                     "Read a terminal pane's output or scrollback. source must be visible, recent, recent_unwrapped, or detection; prefer recent_unwrapped for logs and transcripts.",
	"pane.send_text":                "Send literal text to a terminal pane without pressing Enter. Use pane_send_keys or pane_send_input when terminal execution is intended.",
	"pane.send_keys":                "Send logical key presses to a terminal pane, including Enter, Escape, and Ctrl+C. Use agent_prompt instead for normal coding-agent tasks.",
	"pane.send_input":               "Send text and/or logical keys to a terminal pane in one call. This is raw terminal control and can interleave with an active agent turn.",
	"pane.wait_for_output":          "Wait until a terminal pane's selected output snapshot matches text or a regex. Use pane_read to inspect the matching output.",
	"events.wait":                   "Wait for one supported Herdr event, currently pane agent-status changes. Use it when coordinating state transitions across panes.",
	"notification.show":             "Show a desktop notification in the active Herdr session.",
	"server.agent_manifests":        "List installed agent-detection manifests and the harness kinds that agent_start can launch.",
	"server.reload_agent_manifests": "Reload agent-detection manifests after changing their configuration.",
}
