// Copyright (c) KubeVault Authors
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"
)

const (
	// SpokeCertOrganization is the O= value put into every issued spoke cert.
	// The hub uses this to distinguish bootstrap-issued certs from any other
	// client cert that might be presented.
	SpokeCertOrganization = "openbao-spokes"

	// HubCertOrganization is the O= for the hub TLS cert that serves the gRPC
	// proxy port.
	HubCertOrganization = "openbao-hub"

	caCertValidity   = 10 * 365 * 24 * time.Hour // 10 years
	hubCertValidity  = 365 * 24 * time.Hour      // 1 year
	spokeCertDefault = 30 * 24 * time.Hour       // 30 days
)

// CABundle is the spoke-CA root: a self-signed cert plus its private key.
// Both are PEM-encoded so storage and over-the-wire serialization are trivial.
type CABundle struct {
	CertPEM []byte
	KeyPEM  []byte
}

// HubServerCert is the TLS cert presented by the hub on its gRPC listener.
// Signed by the spoke-CA so that spokes only have to trust one root.
type HubServerCert struct {
	CertPEM []byte
	KeyPEM  []byte
}

// GenerateCA creates a fresh self-signed ECDSA P-256 root CA valid for ~10 years.
func GenerateCA() (*CABundle, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ca key: %w", err)
	}

	serial, err := randSerial()
	if err != nil {
		return nil, err
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "openbao-spoke-ca",
			Organization: []string{SpokeCertOrganization},
		},
		NotBefore:             time.Now().Add(-5 * time.Minute),
		NotAfter:              time.Now().Add(caCertValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("self-sign ca: %w", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal ca key: %w", err)
	}

	return &CABundle{
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

// IssueHubServerCert signs a server cert valid for the hub gRPC listener. The
// cert advertises the given DNS names and IP SANs.
func (ca *CABundle) IssueHubServerCert(dnsNames []string, ipSANs []string) (*HubServerCert, error) {
	caCert, caKey, err := ca.parse()
	if err != nil {
		return nil, err
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	serial, err := randSerial()
	if err != nil {
		return nil, err
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "openbao-hub",
			Organization: []string{HubCertOrganization},
		},
		NotBefore:   time.Now().Add(-5 * time.Minute),
		NotAfter:    time.Now().Add(hubCertValidity),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:    dnsNames,
		IPAddresses: parseIPs(ipSANs),
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("sign hub cert: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}

	return &HubServerCert{
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

// SignSpokeCSR verifies a spoke-submitted CSR, enforces the requested CN, and
// returns a signed client cert valid for `validity`. The CN is the
// authoritative spoke identity used by the proxy gRPC server.
func (ca *CABundle) SignSpokeCSR(csrDER []byte, expectedCN string, validity time.Duration) ([]byte, error) {
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return nil, fmt.Errorf("parse CSR: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("CSR signature: %w", err)
	}
	if csr.Subject.CommonName != expectedCN {
		return nil, fmt.Errorf("CSR CN %q does not match expected %q",
			csr.Subject.CommonName, expectedCN)
	}

	caCert, caKey, err := ca.parse()
	if err != nil {
		return nil, err
	}

	if validity <= 0 {
		validity = spokeCertDefault
	}
	serial, err := randSerial()
	if err != nil {
		return nil, err
	}

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   expectedCN,
			Organization: []string{SpokeCertOrganization},
		},
		NotBefore:   time.Now().Add(-5 * time.Minute),
		NotAfter:    time.Now().Add(validity),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, csr.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("sign spoke cert: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// parse decodes the CA cert + key from PEM. Cached lazily would be nicer, but
// the CA is touched at most once per join, so we keep this stateless.
func (ca *CABundle) parse() (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certBlock, _ := pem.Decode(ca.CertPEM)
	if certBlock == nil {
		return nil, nil, fmt.Errorf("ca cert PEM is empty or malformed")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse ca cert: %w", err)
	}

	keyBlock, _ := pem.Decode(ca.KeyPEM)
	if keyBlock == nil {
		return nil, nil, fmt.Errorf("ca key PEM is empty or malformed")
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse ca key: %w", err)
	}
	return cert, key, nil
}

// ParseCert returns the parsed leaf cert from a PEM-encoded chain (first block).
func ParseCert(certPEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, fmt.Errorf("certificate PEM is empty or malformed")
	}
	return x509.ParseCertificate(block.Bytes)
}

func randSerial() (*big.Int, error) {
	max := new(big.Int).Lsh(big.NewInt(1), 128)
	return rand.Int(rand.Reader, max)
}

func parseIPs(s []string) []net.IP {
	out := make([]net.IP, 0, len(s))
	for _, x := range s {
		if ip := net.ParseIP(x); ip != nil {
			out = append(out, ip)
		}
	}
	return out
}
