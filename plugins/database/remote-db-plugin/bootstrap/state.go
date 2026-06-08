// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: LicenseRef-AppsCode-Free-Trial-1.0.0

package bootstrap

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"sync"
)

// HubState is the runtime hub identity shared between the logical backend
// (which mutates it during `bao agent init` and CA rotation) and the proxy
// gRPC server (which reads it when configuring its TLS listener).
//
// We keep it as a package-level singleton because the logical backend runs in
// the same process as the database-plugin proxy: both are compiled into the
// `bao` binary via helper/builtinplugins/registry.go. A singleton is the
// cheapest IPC available.
type HubState struct {
	mu sync.RWMutex

	caCertPEM    []byte // spoke-CA root, distributed to spokes
	hubCertPEM   []byte // hub TLS cert (signed by spoke-CA)
	hubKeyPEM    []byte
	clientCAPool *x509.CertPool // pool used by the proxy mTLS listener
	hubTLSCert   *tls.Certificate
}

var globalHubState = &HubState{}

// Global returns the process-wide hub state.
func Global() *HubState { return globalHubState }

// SetIdentity replaces the hub's CA + server cert. Called by the logical
// backend on `agent/ca/init` and again on CA rotation. Safe to call before any
// gRPC connection arrives; the proxy listener reads via TLSConfig callbacks
// every handshake.
func (s *HubState) SetIdentity(ca *CABundle, hub *HubServerCert) error {
	if ca == nil || hub == nil {
		return fmt.Errorf("nil CA or hub cert")
	}
	tlsCert, err := tls.X509KeyPair(hub.CertPEM, hub.KeyPEM)
	if err != nil {
		return fmt.Errorf("load hub TLS cert: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.CertPEM) {
		return fmt.Errorf("ca PEM did not yield any usable certs")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.caCertPEM = append([]byte(nil), ca.CertPEM...)
	s.hubCertPEM = append([]byte(nil), hub.CertPEM...)
	s.hubKeyPEM = append([]byte(nil), hub.KeyPEM...)
	s.clientCAPool = pool
	s.hubTLSCert = &tlsCert
	return nil
}

// CACertPEM returns a copy of the spoke-CA cert PEM. Empty before init.
func (s *HubState) CACertPEM() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]byte(nil), s.caCertPEM...)
}

// Ready reports whether SetIdentity has been called successfully.
func (s *HubState) Ready() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hubTLSCert != nil
}

// TLSConfig returns a server TLS config suitable for grpc.NewServer with
// mTLS enabled. The returned config reads `s` on every handshake, so identity
// rotation takes effect on the next connection without restarting the server.
func (s *HubState) TLSConfig() *tls.Config {
	return &tls.Config{
		ClientAuth: tls.RequireAndVerifyClientCert,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			s.mu.RLock()
			defer s.mu.RUnlock()
			if s.hubTLSCert == nil {
				return nil, fmt.Errorf("hub identity not initialized; run `bao agent init`")
			}
			return s.hubTLSCert, nil
		},
		ClientCAs: nil, // see GetConfigForClient
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
			s.mu.RLock()
			defer s.mu.RUnlock()
			if s.clientCAPool == nil {
				return nil, fmt.Errorf("spoke CA not initialized")
			}
			return &tls.Config{
				ClientAuth:   tls.RequireAndVerifyClientCert,
				ClientCAs:    s.clientCAPool,
				Certificates: []tls.Certificate{*s.hubTLSCert},
				MinVersion:   tls.VersionTLS12,
			}, nil
		},
		MinVersion: tls.VersionTLS12,
	}
}
