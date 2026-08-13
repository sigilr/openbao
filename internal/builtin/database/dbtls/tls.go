// Copyright (c) OpenBao a Series of LF Projects, LLC
// SPDX-License-Identifier: MPL-2.0

// Package dbtls converts database plugin configuration fields into an
// in-memory TLS configuration. Keeping certificate material in memory avoids
// requiring operators to mount per-database secrets into the OpenBao process.
package dbtls

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"

	"github.com/go-viper/mapstructure/v2"
)

// Config is the common inline TLS surface accepted by database plugins.
type Config struct {
	CA          string `mapstructure:"tls_ca"`
	Certificate string `mapstructure:"tls_certificate"`
	Key         string `mapstructure:"tls_key"`
	ServerName  string `mapstructure:"tls_server_name"`
	Insecure    bool   `mapstructure:"insecure"`
	UseTLS      bool   `mapstructure:"use_tls"`
}

// Decode reads the common TLS fields from a database plugin configuration.
func Decode(raw map[string]any) (Config, error) {
	var config Config
	if err := mapstructure.WeakDecode(raw, &config); err != nil {
		return Config{}, err
	}
	if (config.Certificate == "") != (config.Key == "") {
		return Config{}, errors.New("both tls_certificate and tls_key are required for mutual TLS")
	}
	return config, nil
}

// Configured reports whether the plugin needs to construct a TLS transport.
func (c Config) Configured() bool {
	return c.UseTLS || c.CA != "" || c.Certificate != "" || c.ServerName != "" || c.Insecure
}

// Build constructs a TLS 1.2+ client configuration from inline PEM data.
func (c Config) Build(defaultServerName string) (*tls.Config, error) {
	serverName := c.ServerName
	if serverName == "" {
		serverName = defaultServerName
	}

	config := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		ServerName:         serverName,
		InsecureSkipVerify: c.Insecure, //nolint:gosec // explicit operator opt-in
	}
	if c.CA != "" {
		roots, err := x509.SystemCertPool()
		if err != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM([]byte(c.CA)) {
			return nil, errors.New("failed to parse tls_ca PEM")
		}
		config.RootCAs = roots
	}
	if c.Certificate != "" {
		certificate, err := tls.X509KeyPair([]byte(c.Certificate), []byte(c.Key))
		if err != nil {
			return nil, fmt.Errorf("failed to parse tls_certificate/tls_key: %w", err)
		}
		config.Certificates = []tls.Certificate{certificate}
	}
	return config, nil
}
