// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/hashicorp/cli"
	"github.com/openbao/openbao/api/v2"
	"github.com/posener/complete"
)

// AgentInitCommand initializes the hub's trust-bootstrap state and prints the
// `bao agent join` invocation to run on each spoke. This is the kubeadm-style
// counterpart to `kubeadm init`.
type AgentInitCommand struct {
	*BaseCommand

	flagMount         string
	flagHubEndpoint   string
	flagHubDNSSANs    []string
	flagHubIPSANs     []string
	flagAllowedSpoke  string
	flagTokenTTL      string
	flagDescription   string
	flagForce         bool
	flagPrintJoinOnly bool
}

var (
	_ cli.Command             = (*AgentInitCommand)(nil)
	_ cli.CommandAutocomplete = (*AgentInitCommand)(nil)
)

func (c *AgentInitCommand) Synopsis() string {
	return "Initialize the hub for spoke joins and print a join command"
}

func (c *AgentInitCommand) Help() string {
	helpText := `
Usage: bao agent init [options]

  Initializes the hub side of the OpenBao hub-and-spoke remote database plugin.
  This command:

    1. Mounts the 'agent/' backend if it is not already mounted.
    2. Generates a self-signed spoke certificate authority and a hub TLS cert
       (unless already initialized; pass -force to regenerate).
    3. Creates a short-lived bootstrap token.
    4. Prints a 'bao agent join' command to run on each spoke.

  The hub TLS cert is presented by the proxy gRPC listener. Spokes verify it
  using the SPKI pin printed in the join command.

  Example:

      $ bao agent init \
          -hub-endpoint=hub.example.com:50053 \
          -hub-dns-sans=hub.example.com \
          -allowed-spoke-name=spoke-1

` + c.Flags().Help()
	return strings.TrimSpace(helpText)
}

func (c *AgentInitCommand) Flags() *FlagSets {
	set := c.flagSet(FlagSetHTTP)
	f := set.NewFlagSet("Command Options")

	f.StringVar(&StringVar{
		Name:    "mount",
		Target:  &c.flagMount,
		Default: "agent",
		Usage:   "Mount path of the agent backend.",
	})
	f.StringVar(&StringVar{
		Name:    "hub-endpoint",
		Target:  &c.flagHubEndpoint,
		Default: "",
		Usage:   "host:port the proxy gRPC server advertises to spokes (required).",
	})
	f.StringSliceVar(&StringSliceVar{
		Name:    "hub-dns-sans",
		Target:  &c.flagHubDNSSANs,
		Default: nil,
		Usage:   "DNS SANs to include on the hub TLS cert.",
	})
	f.StringSliceVar(&StringSliceVar{
		Name:    "hub-ip-sans",
		Target:  &c.flagHubIPSANs,
		Default: nil,
		Usage:   "IP SANs to include on the hub TLS cert.",
	})
	f.StringVar(&StringVar{
		Name:    "allowed-spoke-name",
		Target:  &c.flagAllowedSpoke,
		Default: "",
		Usage:   "Restrict the printed token to a specific spoke identity.",
	})
	f.StringVar(&StringVar{
		Name:    "token-ttl",
		Target:  &c.flagTokenTTL,
		Default: "24h",
		Usage:   "Bootstrap token lifetime.",
	})
	f.StringVar(&StringVar{
		Name:    "description",
		Target:  &c.flagDescription,
		Default: "",
		Usage:   "Free-form description recorded with the token.",
	})
	f.BoolVar(&BoolVar{
		Name:    "force",
		Target:  &c.flagForce,
		Default: false,
		Usage:   "Regenerate the CA + hub cert even if one already exists.",
	})
	f.BoolVar(&BoolVar{
		Name:    "print-join-only",
		Target:  &c.flagPrintJoinOnly,
		Default: false,
		Usage:   "Skip CA init; only create a token and print a join command.",
	})

	return set
}

func (c *AgentInitCommand) AutocompleteArgs() complete.Predictor    { return nil }
func (c *AgentInitCommand) AutocompleteFlags() complete.Flags       { return c.Flags().Completions() }

func (c *AgentInitCommand) Run(args []string) int {
	f := c.Flags()
	if err := f.Parse(args); err != nil {
		c.UI.Error(err.Error())
		return 1
	}
	if c.flagHubEndpoint == "" && !c.flagPrintJoinOnly {
		c.UI.Error("-hub-endpoint is required for first-time init")
		return 1
	}

	client, err := c.Client()
	if err != nil {
		c.UI.Error(err.Error())
		return 2
	}

	mount := strings.TrimSuffix(c.flagMount, "/")

	if !c.flagPrintJoinOnly {
		if err := ensureAgentMount(client, mount); err != nil {
			c.UI.Error(fmt.Sprintf("Mounting %s/: %s", mount, err))
			return 2
		}

		caData, err := initOrFetchCA(client, mount, c)
		if err != nil {
			c.UI.Error(fmt.Sprintf("CA init: %s", err))
			return 2
		}
		c.UI.Info(fmt.Sprintf("Hub identity ready (hub_endpoint=%s)", caData["hub_endpoint"]))
	}

	tokenData, hubEndpoint, caHash, err := createBootstrapToken(client, mount, c)
	if err != nil {
		c.UI.Error(fmt.Sprintf("Create token: %s", err))
		return 2
	}

	c.UI.Output("")
	c.UI.Output("Hub initialized. Run the following on each spoke:")
	c.UI.Output("")
	c.UI.Output(fmt.Sprintf("  bao agent join \\"))
	c.UI.Output(fmt.Sprintf("      -hub-addr=%s \\", hubEndpoint))
	c.UI.Output(fmt.Sprintf("      -hub-cert-hash=%s \\", caHash))
	c.UI.Output(fmt.Sprintf("      -token=%s \\", tokenData["token"]))
	if c.flagAllowedSpoke != "" {
		c.UI.Output(fmt.Sprintf("      -spoke-name=%s", c.flagAllowedSpoke))
	} else {
		c.UI.Output(fmt.Sprintf("      -spoke-name=<choose-a-name>"))
	}
	c.UI.Output("")
	return 0
}

func ensureAgentMount(client *api.Client, mount string) error {
	mounts, err := client.Sys().ListMounts()
	if err != nil {
		return err
	}
	if _, ok := mounts[mount+"/"]; ok {
		return nil
	}
	return client.Sys().Mount(mount+"/", &api.MountInput{
		Type:        "agent",
		Description: "OpenBao hub-and-spoke trust-bootstrap state",
	})
}

func initOrFetchCA(client *api.Client, mount string, c *AgentInitCommand) (map[string]interface{}, error) {
	body := map[string]interface{}{
		"hub_endpoint": c.flagHubEndpoint,
		"force":        c.flagForce,
	}
	if len(c.flagHubDNSSANs) > 0 {
		body["hub_dns_sans"] = c.flagHubDNSSANs
	}
	if len(c.flagHubIPSANs) > 0 {
		body["hub_ip_sans"] = c.flagHubIPSANs
	}
	// Try ca/init first. If it returns "already initialized" and -force was not
	// passed, fall back to ca/info — this makes the command idempotent.
	resp, err := client.Logical().Write(mount+"/ca/init", body)
	if err == nil {
		return resp.Data, nil
	}
	if !c.flagForce && isAlreadyInitialized(err) {
		info, infoErr := client.Logical().Read(mount + "/ca/info")
		if infoErr != nil {
			return nil, fmt.Errorf("ca already initialized but info read failed: %w", infoErr)
		}
		if info == nil {
			return nil, errors.New("ca/info returned nothing")
		}
		return info.Data, nil
	}
	return nil, err
}

func createBootstrapToken(client *api.Client, mount string, c *AgentInitCommand) (map[string]interface{}, string, string, error) {
	body := map[string]interface{}{
		"ttl":         c.flagTokenTTL,
		"description": c.flagDescription,
	}
	if c.flagAllowedSpoke != "" {
		body["allowed_spoke_name"] = c.flagAllowedSpoke
	}
	resp, err := client.Logical().Write(mount+"/bootstrap-tokens", body)
	if err != nil {
		return nil, "", "", err
	}
	info, err := client.Logical().Read(mount + "/ca/info")
	if err != nil {
		return nil, "", "", fmt.Errorf("read ca/info: %w", err)
	}
	if info == nil {
		return nil, "", "", errors.New("ca not initialized")
	}
	hubEndpoint, _ := info.Data["hub_endpoint"].(string)
	caHash, _ := info.Data["ca_cert_hash"].(string)
	if hubEndpoint == "" || caHash == "" {
		return nil, "", "", errors.New("ca/info missing hub_endpoint or ca_cert_hash")
	}
	return resp.Data, hubEndpoint, caHash, nil
}

func isAlreadyInitialized(err error) bool {
	return err != nil && strings.Contains(err.Error(), "CA already initialized")
}

// hostPort is a tiny helper so callers can normalize a -hub-endpoint that
// the operator typed without a port. Unused for now, kept for symmetry with
// agent_join.go which does the inverse parse.
func hostPort(s string) (string, string, error) {
	h, p, err := net.SplitHostPort(s)
	if err != nil {
		return "", "", err
	}
	return h, p, nil
}
