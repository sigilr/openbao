// Copyright (c) KubeVault Authors
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"strings"
)

// PinPrefix is the only hash algorithm we support for CA cert pinning, matching
// kubeadm's `--discovery-token-ca-cert-hash sha256:<hex>` flag.
const PinPrefix = "sha256:"

// HashCert returns the kubeadm-compatible pin for cert: lower-case hex of
// SHA-256 over the DER-encoded SubjectPublicKeyInfo, prefixed with "sha256:".
//
// We hash the SPKI (not the full cert) so a CA rotated to a new cert with the
// same key still validates — the public key is the trust anchor.
func HashCert(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return PinPrefix + strings.ToLower(hex.EncodeToString(sum[:]))
}

// VerifyPin checks that cert matches a pin produced by HashCert.
func VerifyPin(cert *x509.Certificate, pin string) error {
	if !strings.HasPrefix(pin, PinPrefix) {
		return fmt.Errorf("pin %q missing %q prefix", pin, PinPrefix)
	}
	expected := strings.ToLower(strings.TrimPrefix(pin, PinPrefix))
	actual := strings.TrimPrefix(HashCert(cert), PinPrefix)
	if expected != actual {
		return fmt.Errorf("hub CA cert SPKI hash %s does not match pin %s", actual, expected)
	}
	return nil
}
