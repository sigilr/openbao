// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package rabbitmq

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"reflect"
	"testing"
	"time"

	"github.com/openbao/openbao/sdk/v2/logical"
)

// generateTestCert returns a self-signed PEM-encoded certificate and its
// PEM-encoded private key, for use as tls_ca/tls_certificate/tls_key test
// fixtures.
func generateTestCert(t *testing.T) (certPEM, keyPEM string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "rabbitmq-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		IsCA:         true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("failed to create certificate: %v", err)
	}

	certBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("failed to marshal key: %v", err)
	}
	keyBytes := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	return string(certBytes), string(keyBytes)
}

func TestBackend_ConfigConnection_DefaultUsernameTemplate(t *testing.T) {
	var resp *logical.Response
	var err error
	config := logical.TestBackendConfig()
	config.StorageView = &logical.InmemStorage{}
	b := Backend()
	if err = b.Setup(t.Context(), config); err != nil {
		t.Fatal(err)
	}

	configData := map[string]any{
		"connection_uri":    "uri",
		"username":          "username",
		"password":          "password",
		"verify_connection": "false",
	}
	configReq := &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "config/connection",
		Storage:   config.StorageView,
		Data:      configData,
	}
	resp, err = b.HandleRequest(t.Context(), configReq)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("bad: resp: %#v\nerr:%s", resp, err)
	}
	if resp != nil {
		t.Fatal("expected a nil response")
	}

	actualConfig, err := readConfig(t.Context(), config.StorageView)
	if err != nil {
		t.Fatalf("unable to read configuration: %v", err)
	}

	expectedConfig := connectionConfig{
		URI:              "uri",
		Username:         "username",
		Password:         "password",
		UsernameTemplate: "",
	}

	if !reflect.DeepEqual(actualConfig, expectedConfig) {
		t.Fatalf("Expected: %#v\nActual: %#v", expectedConfig, actualConfig)
	}
}

func TestMakeTLSConfig(t *testing.T) {
	certPEM, keyPEM := generateTestCert(t)

	t.Run("no TLS fields set returns nil config", func(t *testing.T) {
		tlsConfig, err := makeTLSConfig(connectionConfig{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if tlsConfig != nil {
			t.Fatalf("expected nil tls.Config, got %#v", tlsConfig)
		}
	})

	t.Run("insecure alone builds an InsecureSkipVerify config", func(t *testing.T) {
		tlsConfig, err := makeTLSConfig(connectionConfig{Insecure: true})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if tlsConfig == nil || !tlsConfig.InsecureSkipVerify {
			t.Fatalf("expected InsecureSkipVerify config, got %#v", tlsConfig)
		}
	})

	t.Run("valid tls_ca is parsed into RootCAs", func(t *testing.T) {
		tlsConfig, err := makeTLSConfig(connectionConfig{TLSCA: certPEM})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if tlsConfig == nil || tlsConfig.RootCAs == nil {
			t.Fatalf("expected RootCAs to be populated, got %#v", tlsConfig)
		}
	})

	t.Run("invalid tls_ca PEM errors", func(t *testing.T) {
		if _, err := makeTLSConfig(connectionConfig{TLSCA: "not a pem bundle"}); err == nil {
			t.Fatal("expected an error for invalid tls_ca PEM")
		}
	})

	t.Run("valid tls_certificate/tls_key pair is parsed", func(t *testing.T) {
		tlsConfig, err := makeTLSConfig(connectionConfig{TLSCertificate: certPEM, TLSKey: keyPEM})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if tlsConfig == nil || len(tlsConfig.Certificates) != 1 {
			t.Fatalf("expected one client certificate, got %#v", tlsConfig)
		}
	})

	t.Run("tls_certificate without tls_key errors", func(t *testing.T) {
		if _, err := makeTLSConfig(connectionConfig{TLSCertificate: certPEM}); err == nil {
			t.Fatal("expected an error when tls_key is missing")
		}
	})

	t.Run("tls_key without tls_certificate errors", func(t *testing.T) {
		if _, err := makeTLSConfig(connectionConfig{TLSKey: keyPEM}); err == nil {
			t.Fatal("expected an error when tls_certificate is missing")
		}
	})

	t.Run("mismatched tls_certificate/tls_key errors", func(t *testing.T) {
		_, otherKeyPEM := generateTestCert(t)
		if _, err := makeTLSConfig(connectionConfig{TLSCertificate: certPEM, TLSKey: otherKeyPEM}); err == nil {
			t.Fatal("expected an error for a mismatched certificate/key pair")
		}
	})
}

func TestBackend_ConfigConnection_TLSFields(t *testing.T) {
	certPEM, keyPEM := generateTestCert(t)

	config := logical.TestBackendConfig()
	config.StorageView = &logical.InmemStorage{}
	b := Backend()
	if err := b.Setup(t.Context(), config); err != nil {
		t.Fatal(err)
	}

	configData := map[string]any{
		"connection_uri":    "uri",
		"username":          "username",
		"password":          "password",
		"verify_connection": "false",
		"tls_ca":            certPEM,
		"tls_certificate":   certPEM,
		"tls_key":           keyPEM,
		"insecure":          "true",
	}
	configReq := &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "config/connection",
		Storage:   config.StorageView,
		Data:      configData,
	}
	resp, err := b.HandleRequest(t.Context(), configReq)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("bad: resp: %#v\nerr:%s", resp, err)
	}

	actualConfig, err := readConfig(t.Context(), config.StorageView)
	if err != nil {
		t.Fatalf("unable to read configuration: %v", err)
	}

	expectedConfig := connectionConfig{
		URI:            "uri",
		Username:       "username",
		Password:       "password",
		TLSCA:          certPEM,
		TLSCertificate: certPEM,
		TLSKey:         keyPEM,
		Insecure:       true,
	}

	if !reflect.DeepEqual(actualConfig, expectedConfig) {
		t.Fatalf("Expected: %#v\nActual: %#v", expectedConfig, actualConfig)
	}
}

func TestBackend_ConfigConnection_InvalidTLSCA(t *testing.T) {
	config := logical.TestBackendConfig()
	config.StorageView = &logical.InmemStorage{}
	b := Backend()
	if err := b.Setup(t.Context(), config); err != nil {
		t.Fatal(err)
	}

	configReq := &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "config/connection",
		Storage:   config.StorageView,
		Data: map[string]any{
			"connection_uri":    "uri",
			"username":          "username",
			"password":          "password",
			"verify_connection": "false",
			"tls_ca":            "not a pem bundle",
		},
	}
	_, err := b.HandleRequest(t.Context(), configReq)
	if err == nil {
		t.Fatal("expected an error for invalid tls_ca")
	}
}

func TestBackend_ConfigConnection_CustomUsernameTemplate(t *testing.T) {
	var resp *logical.Response
	var err error
	config := logical.TestBackendConfig()
	config.StorageView = &logical.InmemStorage{}
	b := Backend()
	if err = b.Setup(t.Context(), config); err != nil {
		t.Fatal(err)
	}

	configData := map[string]any{
		"connection_uri":    "uri",
		"username":          "username",
		"password":          "password",
		"verify_connection": "false",
		"username_template": "{{ .DisplayName }}",
	}
	configReq := &logical.Request{
		Operation: logical.UpdateOperation,
		Path:      "config/connection",
		Storage:   config.StorageView,
		Data:      configData,
	}
	resp, err = b.HandleRequest(t.Context(), configReq)
	if err != nil || (resp != nil && resp.IsError()) {
		t.Fatalf("bad: resp: %#v\nerr:%s", resp, err)
	}
	if resp != nil {
		t.Fatal("expected a nil response")
	}

	actualConfig, err := readConfig(t.Context(), config.StorageView)
	if err != nil {
		t.Fatalf("unable to read configuration: %v", err)
	}

	expectedConfig := connectionConfig{
		URI:              "uri",
		Username:         "username",
		Password:         "password",
		UsernameTemplate: "{{ .DisplayName }}",
	}

	if !reflect.DeepEqual(actualConfig, expectedConfig) {
		t.Fatalf("Expected: %#v\nActual: %#v", expectedConfig, actualConfig)
	}
}
