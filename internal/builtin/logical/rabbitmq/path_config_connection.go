// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package rabbitmq

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"

	cleanhttp "github.com/hashicorp/go-cleanhttp"
	rabbithole "github.com/michaelklishin/rabbit-hole/v3"
	"github.com/openbao/openbao/sdk/v2/framework"
	"github.com/openbao/openbao/sdk/v2/helper/template"
	"github.com/openbao/openbao/sdk/v2/logical"
)

const (
	storageKey = "config/connection"
)

func pathConfigConnection(b *backend) *framework.Path {
	return &framework.Path{
		Pattern: "config/connection",

		DisplayAttrs: &framework.DisplayAttributes{
			OperationPrefix: operationPrefixRabbitMQ,
			OperationVerb:   "configure",
			OperationSuffix: "connection",
		},

		Fields: map[string]*framework.FieldSchema{
			"connection_uri": {
				Type:        framework.TypeString,
				Description: "RabbitMQ Management URI",
			},
			"username": {
				Type:        framework.TypeString,
				Description: "Username of a RabbitMQ management administrator",
			},
			"password": {
				Type:        framework.TypeString,
				Description: "Password of the provided RabbitMQ management user",
			},
			"verify_connection": {
				Type:        framework.TypeBool,
				Default:     true,
				Description: `If set, connection_uri is verified by actually connecting to the RabbitMQ management API`,
			},
			"password_policy": {
				Type:        framework.TypeString,
				Description: "Name of the password policy to use to generate passwords for dynamic credentials.",
			},
			"username_template": {
				Type:        framework.TypeString,
				Description: "Template describing how dynamic usernames are generated.",
			},
			"tls_ca": {
				Type:        framework.TypeString,
				Description: "PEM-encoded CA certificate bundle used to verify the RabbitMQ management API's TLS certificate.",
			},
			"tls_certificate": {
				Type:        framework.TypeString,
				Description: "PEM-encoded client certificate used for mutual TLS to the RabbitMQ management API. Requires tls_key.",
			},
			"tls_key": {
				Type:        framework.TypeString,
				Description: "PEM-encoded private key for tls_certificate.",
			},
			"insecure": {
				Type:        framework.TypeBool,
				Default:     false,
				Description: "Skip TLS certificate verification when connecting to the RabbitMQ management API. Not recommended outside of development.",
			},
		},

		Operations: map[logical.Operation]framework.OperationHandler{
			logical.UpdateOperation: &framework.PathOperation{
				Callback: b.pathConnectionUpdate,
			},
		},

		HelpSynopsis:    pathConfigConnectionHelpSyn,
		HelpDescription: pathConfigConnectionHelpDesc,
	}
}

func (b *backend) pathConnectionUpdate(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	uri := data.Get("connection_uri").(string)
	if uri == "" {
		return logical.ErrorResponse("missing connection_uri"), nil
	}

	username := data.Get("username").(string)
	if username == "" {
		return logical.ErrorResponse("missing username"), nil
	}

	password := data.Get("password").(string)
	if password == "" {
		return logical.ErrorResponse("missing password"), nil
	}

	usernameTemplate := data.Get("username_template").(string)
	if usernameTemplate != "" {
		up, err := template.NewTemplate(template.Template(usernameTemplate))
		if err != nil {
			return logical.ErrorResponse("unable to initialize username template: %w", err), nil
		}

		_, err = up.Generate(UsernameMetadata{})
		if err != nil {
			return logical.ErrorResponse("invalid username template: %w", err), nil
		}
	}

	passwordPolicy := data.Get("password_policy").(string)

	// Store it
	config := connectionConfig{
		URI:              uri,
		Username:         username,
		Password:         password,
		PasswordPolicy:   passwordPolicy,
		UsernameTemplate: usernameTemplate,
		TLSCA:            data.Get("tls_ca").(string),
		TLSCertificate:   data.Get("tls_certificate").(string),
		TLSKey:           data.Get("tls_key").(string),
		Insecure:         data.Get("insecure").(bool),
	}

	client, err := newClient(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create client: %w", err)
	}

	// Don't check the connection_url if verification is disabled
	verifyConnection := data.Get("verify_connection").(bool)
	if verifyConnection {
		// Verify that configured credentials is capable of listing
		if _, err = client.ListUsers(); err != nil {
			return nil, fmt.Errorf("failed to validate the connection: %w", err)
		}
	}

	if err := writeConfig(ctx, req.Storage, config); err != nil {
		return nil, err
	}

	// Reset the client connection
	b.resetClient(ctx)

	return nil, nil
}

func readConfig(ctx context.Context, storage logical.Storage) (connectionConfig, error) {
	entry, err := storage.Get(ctx, storageKey)
	if err != nil {
		return connectionConfig{}, err
	}
	if entry == nil {
		return connectionConfig{}, nil
	}

	var connConfig connectionConfig
	if err := entry.DecodeJSON(&connConfig); err != nil {
		return connectionConfig{}, err
	}
	return connConfig, nil
}

func writeConfig(ctx context.Context, storage logical.Storage, config connectionConfig) error {
	entry, err := logical.StorageEntryJSON(storageKey, config)
	if err != nil {
		return err
	}
	if err := storage.Put(ctx, entry); err != nil {
		return err
	}
	return nil
}

// connectionConfig contains the information required to make a connection to a RabbitMQ node
type connectionConfig struct {
	// URI of the RabbitMQ server
	URI string `json:"connection_uri"`

	// Username which has 'administrator' tag attached to it
	Username string `json:"username"`

	// Password for the Username
	Password string `json:"password"`

	// PasswordPolicy for generating passwords for dynamic credentials
	PasswordPolicy string `json:"password_policy"`

	// UsernameTemplate for storing the raw template in Vault's backing data store
	UsernameTemplate string `json:"username_template"`

	// TLSCA is a PEM-encoded CA certificate bundle used to verify the
	// RabbitMQ management API's TLS certificate.
	TLSCA string `json:"tls_ca"`

	// TLSCertificate and TLSKey are a PEM-encoded client certificate/key
	// pair used for mutual TLS to the RabbitMQ management API.
	TLSCertificate string `json:"tls_certificate"`
	TLSKey         string `json:"tls_key"`

	// Insecure skips TLS certificate verification. Not recommended outside
	// of development.
	Insecure bool `json:"insecure"`
}

// newClient builds a RabbitMQ management API client from the given
// connection config, wiring up TLS (CA bundle, mutual-TLS client identity,
// and/or InsecureSkipVerify) when any of those options are set. It always
// uses a pooled transport so connections don't leak file descriptors.
func newClient(config connectionConfig) (*rabbithole.Client, error) {
	tlsConfig, err := makeTLSConfig(config)
	if err != nil {
		return nil, err
	}

	// Use a default pooled transport so there would be no leaked file descriptors.
	transport := cleanhttp.DefaultPooledTransport()
	if tlsConfig != nil {
		transport.TLSClientConfig = tlsConfig
	}

	client, err := rabbithole.NewTLSClient(config.URI, config.Username, config.Password, transport)
	if err != nil {
		return nil, err
	}
	client.SetTransport(transport)
	return client, nil
}

// makeTLSConfig builds a *tls.Config from the connection config's TLS
// fields. It returns (nil, nil) when none of tls_ca/tls_certificate/tls_key
// /insecure are set, so callers fall back to a plain HTTP(S) client with the
// Go default TLS behavior.
func makeTLSConfig(config connectionConfig) (*tls.Config, error) {
	if config.TLSCA == "" && config.TLSCertificate == "" && config.TLSKey == "" && !config.Insecure {
		return nil, nil
	}

	tlsConfig := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: config.Insecure, //nolint:gosec // operator opt-in via "insecure"
	}

	if config.TLSCA != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(config.TLSCA)) {
			return nil, errors.New("failed to parse tls_ca PEM")
		}
		tlsConfig.RootCAs = pool
	}

	if config.TLSCertificate != "" || config.TLSKey != "" {
		if config.TLSCertificate == "" || config.TLSKey == "" {
			return nil, errors.New("both tls_certificate and tls_key are required to use mutual TLS")
		}
		cert, err := tls.X509KeyPair([]byte(config.TLSCertificate), []byte(config.TLSKey))
		if err != nil {
			return nil, fmt.Errorf("failed to parse tls_certificate/tls_key: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}

	return tlsConfig, nil
}

const pathConfigConnectionHelpSyn = `
Configure the connection URI, username, and password to talk to RabbitMQ management HTTP API.
`

const pathConfigConnectionHelpDesc = `
This path configures the connection properties used to connect to RabbitMQ management HTTP API.
The "connection_uri" parameter is a string that is used to connect to the API. The "username"
and "password" parameters are strings that are used as credentials to the API. The "verify_connection"
parameter is a boolean that is used to verify whether the provided connection URI, username, and password
are valid.

The URI looks like:
"http://localhost:15672"

When connecting over TLS, "tls_ca" may be set to a PEM CA bundle to verify the
management API's server certificate, "tls_certificate" and "tls_key" may be
set to a PEM client certificate/key pair for mutual TLS, and "insecure" may
be set to skip TLS certificate verification entirely (not recommended
outside of development).
`
