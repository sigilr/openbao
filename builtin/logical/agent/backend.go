// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: LicenseRef-AppsCode-Free-Trial-1.0.0

// Package agent implements the OpenBao logical backend that operators use to
// bootstrap and run the hub-and-spoke trust state for the remote-db-plugin.
// It is mounted at `agent/` by `bao agent init`.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	remotedb "github.com/openbao/openbao/plugins/database/remote-db-plugin"
	"github.com/openbao/openbao/plugins/database/remote-db-plugin/bootstrap"
	"github.com/openbao/openbao/sdk/v2/framework"
	"github.com/openbao/openbao/sdk/v2/logical"
)

// AgentBackendFactory builds the `agent/` mount: the trust-bootstrap surface
// `bao agent init` and `bao agent join` talk to.
//
// Wire shape (kubeadm analogue in parens):
//
//	POST   agent/ca/init              — first-time CA generation       (kubeadm init)
//	GET    agent/ca/info              — CA cert + hub TLS endpoint
//	POST   agent/bootstrap-tokens     — create a token, returns id.secret
//	LIST   agent/bootstrap-tokens     — list outstanding tokens
//	GET    agent/bootstrap-tokens/<id>
//	DELETE agent/bootstrap-tokens/<id>
//	GET    agent/cluster-info         — UNAUTH; serves the JWS-signed bundle (cluster-info ConfigMap)
//	POST   agent/sign-csr             — UNAUTH; exchange token for client cert (CSR + bootstrap RBAC)
//
// Factory builds the agent backend. Named Factory to match the convention used
// by every other builtin logical backend (helper/builtinplugins/registry.go
// invokes it as `Factory`).
func Factory(ctx context.Context, conf *logical.BackendConfig) (logical.Backend, error) {
	b := &agentBackend{}
	b.Backend = &framework.Backend{
		Help: strings.TrimSpace(agentBackendHelp),

		PathsSpecial: &logical.Paths{
			Unauthenticated: []string{
				"cluster-info",
				"sign-csr",
			},
			SealWrapStorage: []string{
				agentStorageCA,
				agentStorageTokenPrefix,
			},
		},

		Paths: []*framework.Path{
			b.pathCAInit(),
			b.pathCAInfo(),
			b.pathCARotate(),
			b.pathTokensCreate(),
			b.pathTokenItem(),
			b.pathClusterInfo(),
			b.pathSignCSR(),
			b.pathSpokes(),
		},

		BackendType: logical.TypeLogical,
		InitializeFunc: func(ctx context.Context, req *logical.InitializationRequest) error {
			return b.hydrateHubState(ctx, req.Storage)
		},
	}
	if err := b.Setup(ctx, conf); err != nil {
		return nil, err
	}
	return b, nil
}

const (
	agentStorageCA          = "ca/bundle"
	agentStorageTokenPrefix = "tokens/"

	defaultTokenTTL        = 24 * time.Hour
	defaultSpokeCertExpiry = 30 * 24 * time.Hour

	usageSigning        = "signing"
	usageAuthentication = "authentication"
)

type agentBackend struct {
	*framework.Backend
}

// --- Storage models ---------------------------------------------------------

// caStorage persists the full hub identity: the spoke-CA and the hub's TLS
// cert+key (signed by the spoke-CA). Stored together so a single read on
// startup hydrates the in-memory state shared with the proxy listener.
type caStorage struct {
	CACertPEM   []byte `json:"ca_cert_pem"`
	CAKeyPEM    []byte `json:"ca_key_pem"`
	HubCertPEM  []byte `json:"hub_cert_pem"`
	HubKeyPEM   []byte `json:"hub_key_pem"`
	HubEndpoint string `json:"hub_endpoint"`
	CreatedUnix int64  `json:"created_unix"`
}

// tokenStorage is the kubeadm bootstrap-token equivalent.
//
// Secret is kept in cleartext because we need it to compute the JWS HMAC on
// every cluster-info read — there's no way to verify a signature against a
// hashed secret. The storage is seal-wrapped to mitigate this (see
// PathsSpecial.SealWrapStorage above).
type tokenStorage struct {
	ID               string   `json:"id"`
	Secret           string   `json:"secret"`
	ExpirationUnix   int64    `json:"expiration_unix"`
	AllowedSpokeName string   `json:"allowed_spoke_name,omitempty"`
	Description      string   `json:"description,omitempty"`
	Usages           []string `json:"usages"`
	CreatedUnix      int64    `json:"created_unix"`
}

func (t *tokenStorage) expired() bool {
	return t.ExpirationUnix > 0 && time.Now().Unix() >= t.ExpirationUnix
}

func (t *tokenStorage) hasUsage(want string) bool {
	for _, u := range t.Usages {
		if u == want {
			return true
		}
	}
	return false
}

// --- Hydration --------------------------------------------------------------

// hydrateHubState pushes the persisted CA/hub-cert into the singleton the
// proxy gRPC server reads, then brings up the listener. Called on backend
// init so a restarted OpenBao is immediately ready to receive spoke
// connections without waiting for a database mount to fire.
func (b *agentBackend) hydrateHubState(ctx context.Context, s logical.Storage) error {
	bundle, err := readCA(ctx, s)
	if err != nil {
		return err
	}
	if bundle == nil {
		return nil // not initialized yet; `bao agent init` will populate it
	}
	if err := bootstrap.Global().SetIdentity(
		&bootstrap.CABundle{CertPEM: bundle.CACertPEM, KeyPEM: bundle.CAKeyPEM},
		&bootstrap.HubServerCert{CertPEM: bundle.HubCertPEM, KeyPEM: bundle.HubKeyPEM},
	); err != nil {
		return err
	}
	port, err := portFromEndpoint(bundle.HubEndpoint)
	if err != nil {
		// Older state may have an endpoint without a port; log via the backend's
		// usual error path by returning so the operator sees it on next request.
		return fmt.Errorf("stored hub_endpoint %q has no parseable port: %w", bundle.HubEndpoint, err)
	}
	return remotedb.StartProxyServer(port)
}

// portFromEndpoint extracts the port from "host:port". The hub endpoint is
// validated to have a port by `bao agent init`, so this should not fail in
// fresh state; the explicit error helps when migrating from older data.
func portFromEndpoint(endpoint string) (int, error) {
	_, p, err := net.SplitHostPort(endpoint)
	if err != nil {
		return 0, err
	}
	port, err := strconv.Atoi(p)
	if err != nil {
		return 0, err
	}
	if port <= 0 || port > 65535 {
		return 0, fmt.Errorf("port %d out of range", port)
	}
	return port, nil
}

func readCA(ctx context.Context, s logical.Storage) (*caStorage, error) {
	e, err := s.Get(ctx, agentStorageCA)
	if err != nil {
		return nil, err
	}
	if e == nil {
		return nil, nil
	}
	var c caStorage
	if err := json.Unmarshal(e.Value, &c); err != nil {
		return nil, fmt.Errorf("decode ca bundle: %w", err)
	}
	return &c, nil
}

func writeCA(ctx context.Context, s logical.Storage, c *caStorage) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return s.Put(ctx, &logical.StorageEntry{Key: agentStorageCA, Value: raw})
}

func readToken(ctx context.Context, s logical.Storage, id string) (*tokenStorage, error) {
	e, err := s.Get(ctx, agentStorageTokenPrefix+id)
	if err != nil {
		return nil, err
	}
	if e == nil {
		return nil, nil
	}
	var t tokenStorage
	if err := json.Unmarshal(e.Value, &t); err != nil {
		return nil, fmt.Errorf("decode token: %w", err)
	}
	return &t, nil
}

func writeToken(ctx context.Context, s logical.Storage, t *tokenStorage) error {
	raw, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return s.Put(ctx, &logical.StorageEntry{Key: agentStorageTokenPrefix + t.ID, Value: raw})
}

const agentBackendHelp = `
The agent backend manages the trust-bootstrap state for OpenBao's hub-and-spoke
remote database plugin: the spoke certificate authority, the hub's gRPC server
TLS identity, and short-lived bootstrap tokens issued to operators.

This is the backend that 'bao agent init' and 'bao agent join' talk to. The
'cluster-info' and 'sign-csr' paths are unauthenticated so that a fresh spoke
without an OpenBao token can complete the handshake using only the bootstrap
token printed by 'bao agent init'.
`
