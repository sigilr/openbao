// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/cli"
	"github.com/posener/complete"
)

// AgentListCommand prints the spokes currently connected to the hub's gRPC
// proxy server. This is the operator's "what's running" view.
type AgentListCommand struct {
	*BaseCommand

	flagMount string
}

var (
	_ cli.Command             = (*AgentListCommand)(nil)
	_ cli.CommandAutocomplete = (*AgentListCommand)(nil)
)

func (c *AgentListCommand) Synopsis() string {
	return "List spokes currently connected to the hub"
}

func (c *AgentListCommand) Help() string {
	return strings.TrimSpace(`
Usage: bao agent list [options]

  Lists spokes that have an active Connect stream to the hub's proxy gRPC
  server. The view is point-in-time and may race with a disconnect.

` + c.Flags().Help())
}

func (c *AgentListCommand) Flags() *FlagSets {
	set := c.flagSet(FlagSetHTTP)
	f := set.NewFlagSet("Command Options")
	f.StringVar(&StringVar{Name: "mount", Target: &c.flagMount, Default: "agent",
		Usage: "Mount path of the agent backend."})
	return set
}

func (c *AgentListCommand) AutocompleteArgs() complete.Predictor { return nil }
func (c *AgentListCommand) AutocompleteFlags() complete.Flags    { return c.Flags().Completions() }

func (c *AgentListCommand) Run(args []string) int {
	if err := c.Flags().Parse(args); err != nil {
		c.UI.Error(err.Error())
		return 1
	}
	client, err := c.Client()
	if err != nil {
		c.UI.Error(err.Error())
		return 2
	}
	resp, err := client.Logical().Read(strings.Trim(c.flagMount, "/") + "/spokes")
	if err != nil {
		c.UI.Error(err.Error())
		return 2
	}
	if resp == nil || resp.Data == nil {
		c.UI.Output("(no data)")
		return 0
	}

	port := asUnix(resp.Data["listener_port"])
	if port == 0 {
		c.UI.Output("Proxy gRPC listener is not running.")
		c.UI.Output("Run `bao agent init` on the hub before any spokes can connect.")
		return 0
	}
	c.UI.Output(fmt.Sprintf("Listener: :%d", port))

	count := asUnix(resp.Data["connected_count"])
	healthy := asUnix(resp.Data["healthy_count"])
	stale := asUnix(resp.Data["stale_after_seconds"])
	c.UI.Output(fmt.Sprintf("Connected: %d total, %d healthy (stale after %ds)",
		count, healthy, stale))

	if count == 0 {
		return 0
	}

	rawSpokes, _ := resp.Data["spokes"].([]interface{})
	c.UI.Output("")
	c.UI.Output(fmt.Sprintf("%-20s  %-10s  %-9s  %s", "NAME", "LAST SEEN", "UPTIME", "HEALTH"))
	for _, s := range rawSpokes {
		m, ok := s.(map[string]interface{})
		if !ok {
			continue
		}
		name := str(m["name"])
		lastSeenSecs := asUnix(m["last_seen_seconds"])
		connectedAt := asUnix(m["connected_at_unix"])
		health, _ := m["healthy"].(bool)
		healthStr := "OK"
		if !health {
			healthStr = "STALE"
		}
		uptime := "-"
		if connectedAt > 0 {
			uptime = shortDuration(time.Since(time.Unix(connectedAt, 0)))
		}
		c.UI.Output(fmt.Sprintf("%-20s  %-10s  %-9s  %s",
			name,
			fmt.Sprintf("%ds ago", lastSeenSecs),
			uptime,
			healthStr))
	}
	return 0
}

// shortDuration prints a duration as the largest single unit (e.g. "3d",
// "47m", "12s") for the agent list view. The package's humanDuration formats
// differently and isn't quite what we want here.
func shortDuration(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	default:
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
}
