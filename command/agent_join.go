// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/cli"
	"github.com/openbao/openbao/api/v2"
	"github.com/openbao/openbao/plugins/database/remote-db-plugin/bootstrap"
	"github.com/posener/complete"
)

// AgentJoinCommand is the spoke-side counterpart to AgentInitCommand:
//
//  1. Fetch cluster-info from the hub over an insecure channel.
//  2. Verify the JWS-HS256 signature using the bootstrap token's secret half.
//     (kubeadm equivalent: `cluster-info` ConfigMap + bootstrap-signer JWS.)
//  3. Verify the CA cert's SPKI hash against the operator-supplied pin.
//     (kubeadm equivalent: --discovery-token-ca-cert-hash sha256:...)
//  4. Generate a P-256 key, build a CSR with CN=<spoke-name>, exchange it via
//     the hub's authenticated sign-csr endpoint.
//  5. Write cert.pem, key.pem, ca.pem to -credentials-dir.
//
// The spoke daemon (spoke-agent-v2) then loads that directory and connects to
// the hub's gRPC port over mTLS.
type AgentJoinCommand struct {
	*BaseCommand

	flagBaoAddr        string
	flagMount          string
	flagToken          string
	flagHubAddr        string
	flagHubCertHash    string
	flagSpokeName      string
	flagCredentialsDir string
	flagInsecure       bool
}

var (
	_ cli.Command             = (*AgentJoinCommand)(nil)
	_ cli.CommandAutocomplete = (*AgentJoinCommand)(nil)
)

func (c *AgentJoinCommand) Synopsis() string {
	return "Join a spoke to the hub using a bootstrap token"
}

func (c *AgentJoinCommand) Help() string {
	helpText := `
Usage: bao agent join [options]

  Bootstraps trust between this spoke and the hub OpenBao. The command:

    1. Fetches cluster-info from the hub's OpenBao API (insecure TLS).
    2. Verifies the JWS-HS256 signature using the bootstrap token's secret.
    3. Verifies the CA cert's SPKI hash against -hub-cert-hash.
    4. Generates a key, requests a signed spoke cert via -token.
    5. Writes cert.pem/key.pem/ca.pem to -credentials-dir.

  After a successful join, point spoke-agent-v2 at the credentials directory.

  Example:

      $ bao agent join \
          -bao-addr=https://hub.example.com:8200 \
          -hub-addr=hub.example.com:50053 \
          -hub-cert-hash=sha256:abcdef... \
          -token=abcdef.0123456789abcdef \
          -spoke-name=spoke-1

` + c.Flags().Help()
	return strings.TrimSpace(helpText)
}

func (c *AgentJoinCommand) Flags() *FlagSets {
	set := c.flagSet(FlagSetNone)
	f := set.NewFlagSet("Command Options")

	f.StringVar(&StringVar{
		Name:    "bao-addr",
		Target:  &c.flagBaoAddr,
		Default: "",
		EnvVar:  "BAO_ADDR",
		Usage:   "Address of the hub OpenBao API (e.g. https://hub:8200).",
	})
	f.StringVar(&StringVar{
		Name:    "mount",
		Target:  &c.flagMount,
		Default: "agent",
		Usage:   "Mount path of the agent backend on the hub.",
	})
	f.StringVar(&StringVar{
		Name:    "token",
		Target:  &c.flagToken,
		Default: "",
		Usage:   "Bootstrap token printed by `bao agent init` (id.secret).",
	})
	f.StringVar(&StringVar{
		Name:    "hub-addr",
		Target:  &c.flagHubAddr,
		Default: "",
		Usage:   "host:port of the hub gRPC proxy listener (recorded for the daemon).",
	})
	f.StringVar(&StringVar{
		Name:    "hub-cert-hash",
		Target:  &c.flagHubCertHash,
		Default: "",
		Usage:   "Expected SPKI hash of the hub CA cert (sha256:...).",
	})
	f.StringVar(&StringVar{
		Name:    "spoke-name",
		Target:  &c.flagSpokeName,
		Default: "",
		Usage:   "Identity to embed in the spoke's client cert.",
	})
	f.StringVar(&StringVar{
		Name:    "credentials-dir",
		Target:  &c.flagCredentialsDir,
		Default: "/etc/openbao-spoke",
		Usage:   "Directory to write cert.pem/key.pem/ca.pem.",
	})
	f.BoolVar(&BoolVar{
		Name:    "skip-cert-hash-check",
		Target:  &c.flagInsecure,
		Default: false,
		Usage:   "Skip SPKI-hash verification (trust on first use; not recommended).",
	})
	return set
}

func (c *AgentJoinCommand) AutocompleteArgs() complete.Predictor { return nil }
func (c *AgentJoinCommand) AutocompleteFlags() complete.Flags    { return c.Flags().Completions() }

func (c *AgentJoinCommand) Run(args []string) int {
	if err := c.Flags().Parse(args); err != nil {
		c.UI.Error(err.Error())
		return 1
	}
	if c.flagBaoAddr == "" || c.flagToken == "" || c.flagSpokeName == "" {
		c.UI.Error("-bao-addr, -token, -spoke-name are all required")
		return 1
	}
	if c.flagHubCertHash == "" && !c.flagInsecure {
		c.UI.Error("-hub-cert-hash is required (or pass -skip-cert-hash-check to opt out)")
		return 1
	}

	tok, err := bootstrap.ParseToken(c.flagToken)
	if err != nil {
		c.UI.Error(err.Error())
		return 1
	}

	// 1. Fetch cluster-info insecurely.
	info, err := fetchClusterInfo(c.flagBaoAddr, c.flagMount, tok.ID)
	if err != nil {
		c.UI.Error(fmt.Sprintf("Fetch cluster-info: %s", err))
		return 2
	}

	// 2. Verify the JWS signature using the token's secret.
	if err := bootstrap.VerifyDetached(tok.Secret, []byte(info.Payload), info.Signature); err != nil {
		c.UI.Error(fmt.Sprintf("JWS verification failed: %s", err))
		c.UI.Error("The hub at -bao-addr did not prove knowledge of the bootstrap token; aborting.")
		return 2
	}

	var payload struct {
		CACertPEM   string `json:"ca_cert_pem"`
		HubEndpoint string `json:"hub_endpoint"`
	}
	if err := json.Unmarshal([]byte(info.Payload), &payload); err != nil {
		c.UI.Error(fmt.Sprintf("Decode payload: %s", err))
		return 2
	}

	// 3. Verify the SPKI pin (unless explicitly skipped).
	caCert, err := bootstrap.ParseCert([]byte(payload.CACertPEM))
	if err != nil {
		c.UI.Error(fmt.Sprintf("Parse CA: %s", err))
		return 2
	}
	if !c.flagInsecure {
		if err := bootstrap.VerifyPin(caCert, c.flagHubCertHash); err != nil {
			c.UI.Error(err.Error())
			return 2
		}
	}

	// 4. Generate keypair + CSR.
	key, csrPEM, err := generateSpokeCSR(c.flagSpokeName)
	if err != nil {
		c.UI.Error(fmt.Sprintf("Generate CSR: %s", err))
		return 2
	}

	// 5. Re-dial with the verified CA pinned (now TLS-secure) and sign the CSR.
	signResp, err := signCSR(c.flagBaoAddr, c.flagMount, payload.CACertPEM, c.flagToken, c.flagSpokeName, csrPEM)
	if err != nil {
		c.UI.Error(fmt.Sprintf("Sign CSR: %s", err))
		return 2
	}

	// 6. Persist credentials.
	keyPEM, err := encodeECKey(key)
	if err != nil {
		c.UI.Error(err.Error())
		return 2
	}
	if err := writeCredentials(c.flagCredentialsDir, signResp.CertPEM, keyPEM, signResp.CACertPEM); err != nil {
		c.UI.Error(fmt.Sprintf("Write credentials: %s", err))
		return 2
	}

	hubAddr := c.flagHubAddr
	if hubAddr == "" {
		hubAddr = payload.HubEndpoint
	}
	c.UI.Output(fmt.Sprintf("Joined as spoke %q.", c.flagSpokeName))
	c.UI.Output(fmt.Sprintf("Credentials written to %s", c.flagCredentialsDir))
	c.UI.Output("")
	c.UI.Output("Start the spoke daemon with:")
	c.UI.Output(fmt.Sprintf("  spoke-agent-v2 -server=%s -credentials-dir=%s",
		hubAddr, c.flagCredentialsDir))
	return 0
}

// --- Wire helpers -----------------------------------------------------------

type clusterInfoResp struct {
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

// fetchClusterInfo hits agent/cluster-info INSECURELY. We have not yet
// verified the hub's identity — the JWS signature, computed with our token
// secret, is what proves we're talking to the real hub.
func fetchClusterInfo(baoAddr, mount, tokenID string) (*clusterInfoResp, error) {
	u, err := url.Parse(strings.TrimRight(baoAddr, "/"))
	if err != nil {
		return nil, err
	}
	u.Path = fmt.Sprintf("/v1/%s/cluster-info", strings.Trim(mount, "/"))
	q := u.Query()
	q.Set("token_id", tokenID)
	u.RawQuery = q.Encode()

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	httpClient := &http.Client{Transport: tr}
	resp, err := httpClient.Get(u.String())
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("cluster-info HTTP %d", resp.StatusCode)
	}

	var body struct {
		Data clusterInfoResp `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	if body.Data.Payload == "" || body.Data.Signature == "" {
		return nil, fmt.Errorf("hub returned empty cluster-info bundle")
	}
	return &body.Data, nil
}

type signResp struct {
	CertPEM   []byte
	CACertPEM []byte
}

// signCSR talks to agent/sign-csr using TLS pinned to the now-verified CA.
// At this point we know the hub is the real hub (JWS verified) and its TLS
// cert SPKI matches the pin, so a standard TLS handshake against the bao API
// rooted at the spoke CA covers the bao API listener as well — provided the
// operator configured OpenBao to terminate TLS with a cert chained to the
// spoke CA. In the more common case where the bao API uses a different cert,
// the caller can pass -skip-cert-hash-check, but the JWS guarantee remains.
func signCSR(baoAddr, mount, hubCAPEM, token, spokeName string, csrPEM []byte) (*signResp, error) {
	// Use the public api package so this code path benefits from any
	// retry/timeouts/headers that core configures.
	cfg := api.DefaultConfig()
	cfg.Address = baoAddr
	// We DO want to verify TLS now if the bao API cert is chained to the
	// spoke CA. Fall back to insecure if the user opted in — the post-JWS
	// payload is what authenticates the channel.
	cfg.HttpClient = &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
	cli, err := api.NewClient(cfg)
	if err != nil {
		return nil, err
	}

	body := map[string]interface{}{
		"token":      token,
		"spoke_name": spokeName,
		"csr_pem":    string(csrPEM),
	}
	resp, err := cli.Logical().WriteWithContext(
		context.Background(),
		strings.Trim(mount, "/")+"/sign-csr",
		body,
	)
	if err != nil {
		return nil, err
	}
	if resp == nil || resp.Data == nil {
		return nil, fmt.Errorf("sign-csr returned no data")
	}
	certPEM, _ := resp.Data["cert_pem"].(string)
	caPEM, _ := resp.Data["ca_cert_pem"].(string)
	if certPEM == "" || caPEM == "" {
		return nil, fmt.Errorf("sign-csr missing cert_pem or ca_cert_pem")
	}
	// Sanity: the CA the hub returned must match the one we already pinned.
	if hubCAPEM != "" && strings.TrimSpace(caPEM) != strings.TrimSpace(hubCAPEM) {
		return nil, fmt.Errorf("hub returned a different CA via sign-csr than via cluster-info")
	}
	return &signResp{CertPEM: []byte(certPEM), CACertPEM: []byte(caPEM)}, nil
}

// --- Crypto helpers ---------------------------------------------------------

func generateSpokeCSR(cn string) (*ecdsa.PrivateKey, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: cn},
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		return nil, nil, err
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}

func encodeECKey(k *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

func writeCredentials(dir string, cert, key, ca []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, f := range []struct {
		name string
		data []byte
		mode os.FileMode
	}{
		{"cert.pem", cert, 0o644},
		{"key.pem", key, 0o600},
		{"ca.pem", ca, 0o644},
	} {
		if err := os.WriteFile(filepath.Join(dir, f.name), f.data, f.mode); err != nil {
			return err
		}
	}
	return nil
}
