// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"strings"
	"time"

	remotedb "github.com/openbao/openbao/plugins/database/remote-db-plugin"
	"github.com/openbao/openbao/plugins/database/remote-db-plugin/bootstrap"
	"github.com/openbao/openbao/sdk/v2/framework"
	"github.com/openbao/openbao/sdk/v2/logical"
)

// --- ca/init -----------------------------------------------------------------

func (b *agentBackend) pathCAInit() *framework.Path {
	return &framework.Path{
		Pattern: "ca/init",
		Fields: map[string]*framework.FieldSchema{
			"hub_endpoint": {
				Type:        framework.TypeString,
				Description: "host:port the proxy gRPC listener will advertise to spokes.",
			},
			"hub_dns_sans": {
				Type:        framework.TypeStringSlice,
				Description: "DNS names to include as SANs on the hub TLS cert.",
			},
			"hub_ip_sans": {
				Type:        framework.TypeStringSlice,
				Description: "IPs to include as SANs on the hub TLS cert.",
			},
			"force": {
				Type:        framework.TypeBool,
				Description: "If true, regenerate the CA even if one already exists.",
			},
		},
		Operations: map[logical.Operation]framework.OperationHandler{
			logical.UpdateOperation: &framework.PathOperation{Callback: b.handleCAInit},
		},
		HelpSynopsis: "Initialize the spoke certificate authority and hub TLS identity.",
	}
}

func (b *agentBackend) handleCAInit(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	endpoint := d.Get("hub_endpoint").(string)
	if endpoint == "" {
		return logical.ErrorResponse("hub_endpoint is required"), nil
	}
	port, err := portFromEndpoint(endpoint)
	if err != nil {
		return logical.ErrorResponse(fmt.Sprintf(
			"hub_endpoint must be host:port (%v)", err,
		)), nil
	}
	dnsSANs := d.Get("hub_dns_sans").([]string)
	ipSANs := d.Get("hub_ip_sans").([]string)
	force := d.Get("force").(bool)

	existing, err := readCA(ctx, req.Storage)
	if err != nil {
		return nil, err
	}
	if existing != nil && !force {
		return logical.ErrorResponse("CA already initialized; pass force=true to regenerate"), nil
	}

	ca, err := bootstrap.GenerateCA()
	if err != nil {
		return nil, err
	}
	hub, err := ca.IssueHubServerCert(dnsSANs, ipSANs)
	if err != nil {
		return nil, err
	}

	bundle := &caStorage{
		CACertPEM:   ca.CertPEM,
		CAKeyPEM:    ca.KeyPEM,
		HubCertPEM:  hub.CertPEM,
		HubKeyPEM:   hub.KeyPEM,
		HubEndpoint: endpoint,
		CreatedUnix: time.Now().Unix(),
	}
	if err := writeCA(ctx, req.Storage, bundle); err != nil {
		return nil, err
	}
	if err := bootstrap.Global().SetIdentity(ca, hub); err != nil {
		return nil, err
	}
	// Bring up the gRPC listener now, while we have an authenticated operator
	// holding the response. Doing it here (instead of lazily from the database
	// mount's Initialize) means port problems surface to whoever ran
	// `bao agent init`, not to whoever later mounts a database engine, and the
	// port comes from a single source of truth instead of the first DB mount's
	// agent_port config.
	if err := remotedb.StartProxyServer(port); err != nil {
		return logical.ErrorResponse(fmt.Sprintf("start proxy listener: %v", err)), nil
	}

	caCert, err := bootstrap.ParseCert(ca.CertPEM)
	if err != nil {
		return nil, err
	}
	return &logical.Response{
		Data: map[string]any{
			"ca_cert_pem":  string(ca.CertPEM),
			"ca_cert_hash": bootstrap.HashCert(caCert),
			"hub_endpoint": endpoint,
		},
	}, nil
}

// --- ca/info -----------------------------------------------------------------

func (b *agentBackend) pathCAInfo() *framework.Path {
	return &framework.Path{
		Pattern: "ca/info",
		Operations: map[logical.Operation]framework.OperationHandler{
			logical.ReadOperation: &framework.PathOperation{Callback: b.handleCAInfo},
		},
		HelpSynopsis: "Return CA + hub cert metadata.",
	}
}

func (b *agentBackend) handleCAInfo(ctx context.Context, req *logical.Request, _ *framework.FieldData) (*logical.Response, error) {
	bundle, err := readCA(ctx, req.Storage)
	if err != nil {
		return nil, err
	}
	if bundle == nil {
		return logical.ErrorResponse("CA not initialized; run `bao agent init`"), nil
	}
	caCert, err := bootstrap.ParseCert(bundle.CACertPEM)
	if err != nil {
		return nil, err
	}
	hubCert, err := bootstrap.ParseCert(bundle.HubCertPEM)
	if err != nil {
		return nil, err
	}

	ipSANs := make([]string, 0, len(hubCert.IPAddresses))
	for _, ip := range hubCert.IPAddresses {
		ipSANs = append(ipSANs, ip.String())
	}

	return &logical.Response{
		Data: map[string]any{
			"ca_cert_pem":        string(bundle.CACertPEM),
			"ca_cert_hash":       bootstrap.HashCert(caCert),
			"ca_subject":         caCert.Subject.String(),
			"ca_not_after":       caCert.NotAfter.Unix(),
			"hub_endpoint":       bundle.HubEndpoint,
			"hub_cert_subject":   hubCert.Subject.String(),
			"hub_cert_not_after": hubCert.NotAfter.Unix(),
			"hub_dns_sans":       hubCert.DNSNames,
			"hub_ip_sans":        ipSANs,
			"created_unix":       bundle.CreatedUnix,
			"listener_port":      remotedb.ProxyServerPort(),
		},
	}, nil
}

// --- ca/rotate ---------------------------------------------------------------

func (b *agentBackend) pathCARotate() *framework.Path {
	return &framework.Path{
		Pattern: "ca/rotate",
		Fields: map[string]*framework.FieldSchema{
			"full": {
				Type:        framework.TypeBool,
				Description: "If true, rotate the spoke-CA itself (invalidates all spoke certs).",
			},
			"hub_dns_sans": {
				Type:        framework.TypeStringSlice,
				Description: "Override DNS SANs on the new hub cert; defaults to existing.",
			},
			"hub_ip_sans": {
				Type:        framework.TypeStringSlice,
				Description: "Override IP SANs on the new hub cert; defaults to existing.",
			},
		},
		Operations: map[logical.Operation]framework.OperationHandler{
			logical.UpdateOperation: &framework.PathOperation{Callback: b.handleCARotate},
		},
		HelpSynopsis: "Rotate the hub TLS cert (default) or the entire spoke-CA.",
	}
}

func (b *agentBackend) handleCARotate(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	bundle, err := readCA(ctx, req.Storage)
	if err != nil {
		return nil, err
	}
	if bundle == nil {
		return logical.ErrorResponse("CA not initialized; run `bao agent init`"), nil
	}

	full := d.Get("full").(bool)
	dnsSANs := d.Get("hub_dns_sans").([]string)
	ipSANs := d.Get("hub_ip_sans").([]string)

	// Carry forward whatever was on the existing hub cert if the operator
	// didn't override. Rotation should not silently drop SANs.
	existingHubCert, err := bootstrap.ParseCert(bundle.HubCertPEM)
	if err != nil {
		return nil, err
	}
	if len(dnsSANs) == 0 {
		dnsSANs = existingHubCert.DNSNames
	}
	if len(ipSANs) == 0 {
		ipSANs = make([]string, 0, len(existingHubCert.IPAddresses))
		for _, ip := range existingHubCert.IPAddresses {
			ipSANs = append(ipSANs, ip.String())
		}
	}

	var (
		newCA       *bootstrap.CABundle
		newHub      *bootstrap.HubServerCert
		rotatedKind string
	)
	if full {
		newCA, err = bootstrap.GenerateCA()
		if err != nil {
			return nil, err
		}
		rotatedKind = "ca+hub"
	} else {
		newCA = &bootstrap.CABundle{CertPEM: bundle.CACertPEM, KeyPEM: bundle.CAKeyPEM}
		rotatedKind = "hub"
	}
	newHub, err = newCA.IssueHubServerCert(dnsSANs, ipSANs)
	if err != nil {
		return nil, err
	}

	updated := &caStorage{
		CACertPEM:   newCA.CertPEM,
		CAKeyPEM:    newCA.KeyPEM,
		HubCertPEM:  newHub.CertPEM,
		HubKeyPEM:   newHub.KeyPEM,
		HubEndpoint: bundle.HubEndpoint, // endpoint never changes via rotate
		CreatedUnix: bundle.CreatedUnix,
	}
	if err := writeCA(ctx, req.Storage, updated); err != nil {
		return nil, err
	}
	if err := bootstrap.Global().SetIdentity(newCA, newHub); err != nil {
		return nil, err
	}

	caCert, err := bootstrap.ParseCert(newCA.CertPEM)
	if err != nil {
		return nil, err
	}
	resp := &logical.Response{
		Data: map[string]any{
			"rotated":      rotatedKind,
			"ca_cert_hash": bootstrap.HashCert(caCert),
			"ca_cert_pem":  string(newCA.CertPEM),
		},
	}
	if full {
		resp.AddWarning("Full CA rotation invalidates every issued spoke cert. Active spoke streams stay up until they disconnect (TLS auth happens at handshake), but any reconnect will fail. Re-run `bao agent join` on each spoke with a fresh bootstrap token, then restart the spoke daemon.")
	}
	return resp, nil
}

// --- bootstrap-tokens (create + list) ----------------------------------------

func (b *agentBackend) pathTokensCreate() *framework.Path {
	return &framework.Path{
		Pattern: "bootstrap-tokens/?$",
		Fields: map[string]*framework.FieldSchema{
			"ttl": {
				Type:        framework.TypeDurationSecond,
				Description: "Token lifetime; defaults to 24h. 0 = never expires.",
			},
			"allowed_spoke_name": {
				Type:        framework.TypeString,
				Description: "If set, the issued spoke cert's CN must equal this value.",
			},
			"description": {
				Type:        framework.TypeString,
				Description: "Free-form description shown in `bao agent token list`.",
			},
			"usages": {
				Type:        framework.TypeStringSlice,
				Description: "Allowed usages; defaults to [signing, authentication].",
			},
		},
		Operations: map[logical.Operation]framework.OperationHandler{
			logical.UpdateOperation: &framework.PathOperation{Callback: b.handleTokenCreate},
			logical.ListOperation:   &framework.PathOperation{Callback: b.handleTokenList},
		},
		HelpSynopsis: "Create or list bootstrap tokens.",
	}
}

func (b *agentBackend) handleTokenCreate(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	ttl := time.Duration(d.Get("ttl").(int)) * time.Second
	if ttl == 0 {
		ttl = defaultTokenTTL
	}
	allowedName := d.Get("allowed_spoke_name").(string)
	description := d.Get("description").(string)
	usages := d.Get("usages").([]string)
	if len(usages) == 0 {
		usages = []string{usageSigning, usageAuthentication}
	}

	tok, err := bootstrap.GenerateToken()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	rec := &tokenStorage{
		ID:               tok.ID,
		Secret:           tok.Secret,
		ExpirationUnix:   now.Add(ttl).Unix(),
		AllowedSpokeName: allowedName,
		Description:      description,
		Usages:           usages,
		CreatedUnix:      now.Unix(),
	}
	if ttl < 0 {
		rec.ExpirationUnix = 0
	}
	if err := writeToken(ctx, req.Storage, rec); err != nil {
		return nil, err
	}
	resp := &logical.Response{
		Data: map[string]any{
			"id":                 tok.ID,
			"token":              tok.String(),
			"expiration_unix":    rec.ExpirationUnix,
			"allowed_spoke_name": allowedName,
			"usages":             usages,
		},
	}
	// The token is the JWS-HMAC key and the spoke-CSR-signing capability all
	// in one short string. Operators need to see it once — same trade-off as
	// `kubeadm token create` — but they should not see it again in audit
	// logs or forwarded responses. Emit a warning so it shows up next to the
	// token wherever the caller surfaces it.
	resp.AddWarning("This token is shown only once. Communicate it out of band; do not store or log it. Configure audit_non_hmac_response_keys=token on the agent mount and request response wrapping (-wrap-ttl) for production use.")
	return resp, nil
}

func (b *agentBackend) handleTokenList(ctx context.Context, req *logical.Request, _ *framework.FieldData) (*logical.Response, error) {
	ids, err := req.Storage.List(ctx, agentStorageTokenPrefix)
	if err != nil {
		return nil, err
	}
	return logical.ListResponse(ids), nil
}

// --- bootstrap-tokens/<id> ---------------------------------------------------

func (b *agentBackend) pathTokenItem() *framework.Path {
	return &framework.Path{
		Pattern: "bootstrap-tokens/" + framework.GenericNameRegex("id"),
		Fields: map[string]*framework.FieldSchema{
			"id": {Type: framework.TypeString, Description: "Token id (6 chars)."},
		},
		Operations: map[logical.Operation]framework.OperationHandler{
			logical.ReadOperation:   &framework.PathOperation{Callback: b.handleTokenRead},
			logical.DeleteOperation: &framework.PathOperation{Callback: b.handleTokenDelete},
		},
	}
}

func (b *agentBackend) handleTokenRead(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	id := d.Get("id").(string)
	t, err := readToken(ctx, req.Storage, id)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, nil
	}
	return &logical.Response{
		Data: map[string]any{
			"id":                 t.ID,
			"expiration_unix":    t.ExpirationUnix,
			"created_unix":       t.CreatedUnix,
			"allowed_spoke_name": t.AllowedSpokeName,
			"description":        t.Description,
			"usages":             t.Usages,
			"expired":            t.expired(),
		},
	}, nil
}

func (b *agentBackend) handleTokenDelete(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	id := d.Get("id").(string)
	if err := req.Storage.Delete(ctx, agentStorageTokenPrefix+id); err != nil {
		return nil, err
	}
	return nil, nil
}

// --- cluster-info (UNAUTH) ---------------------------------------------------

func (b *agentBackend) pathClusterInfo() *framework.Path {
	return &framework.Path{
		Pattern: "cluster-info",
		Fields: map[string]*framework.FieldSchema{
			"token_id": {
				Type:        framework.TypeString,
				Description: "Bootstrap token id; required for the JWS signature.",
				Query:       true,
			},
		},
		Operations: map[logical.Operation]framework.OperationHandler{
			logical.ReadOperation: &framework.PathOperation{Callback: b.handleClusterInfo},
		},
		HelpSynopsis: "Public hub-info bundle, signed with the bootstrap token's secret.",
	}
}

// clusterInfoPayload is what the spoke verifies against the JWS. It must be
// re-marshaled in a deterministic order on both sides; since both sides use
// encoding/json with the same struct, the order is stable.
type clusterInfoPayload struct {
	CACertPEM   string `json:"ca_cert_pem"`
	HubEndpoint string `json:"hub_endpoint"`
}

func (b *agentBackend) handleClusterInfo(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	tokenID := d.Get("token_id").(string)
	if !bootstrap.ValidTokenID(tokenID) {
		// Reject syntactically-bad ids before the storage lookup. The path is
		// unauthenticated and the id space is small (~16M); cheap upfront
		// rejection keeps storage off the hot path for brute-force probes.
		// Pair this with a sys/quotas/rate-limit policy on agent/cluster-info
		// (see DESIGN.md "Hardening").
		return logical.ErrorResponse("token unknown or expired"), nil
	}
	t, err := readToken(ctx, req.Storage, tokenID)
	if err != nil {
		return nil, err
	}
	if t == nil || t.expired() {
		// Returning the same error for both unknown and expired stops a remote
		// caller from enumerating valid ids by timing the response.
		return logical.ErrorResponse("token unknown or expired"), nil
	}
	bundle, err := readCA(ctx, req.Storage)
	if err != nil {
		return nil, err
	}
	if bundle == nil {
		// Collapsing to the same error as token-not-found avoids leaking
		// whether the hub itself has been initialized via this endpoint.
		return logical.ErrorResponse("token unknown or expired"), nil
	}

	payload := clusterInfoPayload{
		CACertPEM:   string(bundle.CACertPEM),
		HubEndpoint: bundle.HubEndpoint,
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	sig, err := bootstrap.SignDetached(bootstrap.Token{ID: t.ID, Secret: t.Secret}, payloadBytes)
	if err != nil {
		return nil, err
	}

	return &logical.Response{
		Data: map[string]any{
			"payload":   string(payloadBytes),
			"signature": sig,
		},
	}, nil
}

// --- sign-csr (UNAUTH, token-authenticated) ----------------------------------

func (b *agentBackend) pathSignCSR() *framework.Path {
	return &framework.Path{
		Pattern: "sign-csr",
		Fields: map[string]*framework.FieldSchema{
			"token": {
				Type:        framework.TypeString,
				Description: "Bootstrap token in <id>.<secret> form.",
			},
			"spoke_name": {
				Type:        framework.TypeString,
				Description: "Identity the spoke is requesting; becomes the cert CN.",
			},
			"csr_pem": {
				Type:        framework.TypeString,
				Description: "PEM-encoded PKCS#10 CSR.",
			},
			"ttl": {
				Type:        framework.TypeDurationSecond,
				Description: "Requested cert validity; capped at 30d if missing.",
			},
		},
		Operations: map[logical.Operation]framework.OperationHandler{
			logical.UpdateOperation: &framework.PathOperation{Callback: b.handleSignCSR},
		},
		HelpSynopsis: "Exchange a bootstrap token for a signed spoke client cert.",
	}
}

func (b *agentBackend) handleSignCSR(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	rawTok := d.Get("token").(string)
	spokeName := d.Get("spoke_name").(string)
	csrPEM := d.Get("csr_pem").(string)
	ttl := time.Duration(d.Get("ttl").(int)) * time.Second
	if ttl <= 0 {
		ttl = defaultSpokeCertExpiry
	}
	if spokeName == "" || csrPEM == "" || rawTok == "" {
		return logical.ErrorResponse("token, spoke_name, csr_pem are all required"), nil
	}

	parsedTok, err := bootstrap.ParseToken(rawTok)
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}

	t, err := readToken(ctx, req.Storage, parsedTok.ID)
	if err != nil {
		return nil, err
	}
	if t == nil || t.expired() {
		return logical.ErrorResponse("token unknown or expired"), nil
	}
	if !bootstrap.ConstantTimeEqualSecret(t.Secret, parsedTok.Secret) {
		return logical.ErrorResponse("token unknown or expired"), nil
	}
	if !t.hasUsage(usageSigning) {
		return logical.ErrorResponse("token does not have the 'signing' usage"), nil
	}
	if t.AllowedSpokeName != "" && t.AllowedSpokeName != spokeName {
		return logical.ErrorResponse(fmt.Sprintf(
			"token is restricted to spoke %q", t.AllowedSpokeName,
		)), nil
	}

	bundle, err := readCA(ctx, req.Storage)
	if err != nil {
		return nil, err
	}
	if bundle == nil {
		return logical.ErrorResponse("hub not initialized"), nil
	}
	ca := &bootstrap.CABundle{CertPEM: bundle.CACertPEM, KeyPEM: bundle.CAKeyPEM}

	csrDER, err := pemDecodeCSR(csrPEM)
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}
	certPEM, err := ca.SignSpokeCSR(csrDER, spokeName, ttl)
	if err != nil {
		return logical.ErrorResponse(err.Error()), nil
	}

	return &logical.Response{
		Data: map[string]any{
			"cert_pem":    string(certPEM),
			"ca_cert_pem": string(bundle.CACertPEM),
		},
	}, nil
}

// --- spokes -----------------------------------------------------------------

func (b *agentBackend) pathSpokes() *framework.Path {
	return &framework.Path{
		Pattern: "spokes",
		Operations: map[logical.Operation]framework.OperationHandler{
			logical.ReadOperation: &framework.PathOperation{Callback: b.handleSpokesList},
			logical.ListOperation: &framework.PathOperation{Callback: b.handleSpokesList},
		},
		HelpSynopsis: "List spokes currently connected to the proxy gRPC server.",
	}
}

func (b *agentBackend) handleSpokesList(_ context.Context, _ *logical.Request, _ *framework.FieldData) (*logical.Response, error) {
	statuses := remotedb.ListConnectedSpokes()
	now := time.Now()
	entries := make([]map[string]any, 0, len(statuses))
	healthyCount := 0
	for _, s := range statuses {
		if s.Healthy {
			healthyCount++
		}
		entries = append(entries, map[string]any{
			"name":              s.Name,
			"connected_at_unix": s.ConnectedAt.Unix(),
			"last_seen_unix":    s.LastSeen.Unix(),
			"last_seen_seconds": int64(now.Sub(s.LastSeen) / time.Second),
			"healthy":           s.Healthy,
		})
	}
	return &logical.Response{
		Data: map[string]any{
			"spokes":              entries,
			"connected_count":     len(statuses),
			"healthy_count":       healthyCount,
			"listener_port":       remotedb.ProxyServerPort(),
			"stale_after_seconds": int64(remotedb.SpokeStaleAfter / time.Second),
		},
	}, nil
}

func pemDecodeCSR(csrPEM string) ([]byte, error) {
	csrPEM = strings.TrimSpace(csrPEM)
	if !strings.HasPrefix(csrPEM, "-----BEGIN") {
		return nil, fmt.Errorf("csr_pem is not PEM-encoded")
	}
	block, _ := pem.Decode([]byte(csrPEM))
	if block == nil {
		return nil, fmt.Errorf("csr_pem could not be decoded")
	}
	if block.Type != "CERTIFICATE REQUEST" && block.Type != "NEW CERTIFICATE REQUEST" {
		return nil, fmt.Errorf("unexpected PEM block %q", block.Type)
	}
	return block.Bytes, nil
}
