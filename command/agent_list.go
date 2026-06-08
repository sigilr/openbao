// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"fmt"
	"strings"

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
	if count == 0 {
		c.UI.Output("No spokes connected.")
		return 0
	}

	names, _ := resp.Data["connected"].([]interface{})
	c.UI.Output(fmt.Sprintf("Connected (%d):", count))
	for _, n := range names {
		if s, ok := n.(string); ok {
			c.UI.Output("  " + s)
		}
	}
	return 0
}
