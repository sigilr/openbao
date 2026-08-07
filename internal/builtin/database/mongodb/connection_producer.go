// Copyright (c) HashiCorp, Inc.
// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

package mongodb

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/mitchellh/mapstructure"
	"github.com/openbao/openbao/sdk/v2/database/helper/connutil"
	"github.com/openbao/openbao/sdk/v2/database/helper/dbutil"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
)

// mongoDBConnectionProducer holds the mounted-engine config and the
// long-lived mongo.Client used by every dbplugin request. It implements its
// own connection management (lock, lazy connect, ping-on-reuse) rather than
// embedding connutil.SQLConnectionProducer because MongoDB is not a SQL
// connection.
type mongoDBConnectionProducer struct {
	ConnectionURL string `json:"connection_url" structs:"connection_url" mapstructure:"connection_url"`
	WriteConcern  string `json:"write_concern" structs:"write_concern" mapstructure:"write_concern"`

	Username string `json:"username" structs:"username" mapstructure:"username"`
	Password string `json:"password" structs:"password" mapstructure:"password"`

	TLSCertificateKeyData []byte `json:"tls_certificate_key" structs:"-" mapstructure:"tls_certificate_key"`
	TLSCAData             []byte `json:"tls_ca"              structs:"-" mapstructure:"tls_ca"`

	SocketTimeout          time.Duration `json:"socket_timeout"           structs:"-" mapstructure:"socket_timeout"`
	ConnectTimeout         time.Duration `json:"connect_timeout"          structs:"-" mapstructure:"connect_timeout"`
	ServerSelectionTimeout time.Duration `json:"server_selection_timeout" structs:"-" mapstructure:"server_selection_timeout"`

	Initialized   bool
	RawConfig     map[string]interface{}
	Type          string
	clientOptions *options.ClientOptions
	client        *mongo.Client
	sync.Mutex
}

// writeConcern is the wire-format config a user can set via
// `write_concern={...}` on the mount. We translate it into mongo
// writeconcern options below.
type writeConcernConfig struct {
	W        int    // Min # of servers to ack before success
	WMode    string // Write mode for MongoDB 2.0+ (e.g. "majority")
	WTimeout int    // Milliseconds to wait for W before timing out
	FSync    bool   // DEPRECATED: handled by J. See https://jira.mongodb.org/browse/CXX-910
	J        bool   // Sync via the journal if present
}

func (c *mongoDBConnectionProducer) loadConfig(cfg map[string]interface{}) error {
	if err := mapstructure.WeakDecode(cfg, c); err != nil {
		return err
	}

	if len(c.ConnectionURL) == 0 {
		return errors.New("connection_url cannot be empty")
	}

	if c.SocketTimeout < 0 {
		return errors.New("socket_timeout must be >= 0")
	}
	if c.ConnectTimeout < 0 {
		return errors.New("connect_timeout must be >= 0")
	}
	if c.ServerSelectionTimeout < 0 {
		return errors.New("server_selection_timeout must be >= 0")
	}

	opts, err := c.makeClientOpts()
	if err != nil {
		return err
	}

	c.clientOptions = opts
	return nil
}

// Connection returns the cached client if Ping succeeds, otherwise discards
// it and creates a fresh one. Holds the mutex.
func (c *mongoDBConnectionProducer) Connection(ctx context.Context) (*mongo.Client, error) {
	if !c.Initialized {
		return nil, connutil.ErrNotInitialized
	}

	c.Lock()
	defer c.Unlock()

	if c.client != nil {
		if err := c.client.Ping(ctx, readpref.Primary()); err == nil {
			return c.client, nil
		}
		_ = c.client.Disconnect(ctx)
	}

	client, err := c.createClient(ctx)
	if err != nil {
		return nil, err
	}
	c.client = client
	return c.client, nil
}

func (c *mongoDBConnectionProducer) createClient(ctx context.Context) (*mongo.Client, error) {
	if !c.Initialized {
		return nil, errors.New("failed to create client: connection producer is not initialized")
	}
	if c.clientOptions == nil {
		return nil, errors.New("missing client options")
	}
	return mongo.Connect(
		ctx,
		options.MergeClientOptions(options.Client().ApplyURI(c.getConnectionURL()), c.clientOptions),
	)
}

// Close disconnects the mongo client. Holds the mutex.
func (c *mongoDBConnectionProducer) Close() error {
	c.Lock()
	defer c.Unlock()

	if c.client != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Minute)
		defer cancel()
		if err := c.client.Disconnect(ctx); err != nil {
			return err
		}
	}

	c.client = nil
	return nil
}

func (c *mongoDBConnectionProducer) secretValues() map[string]string {
	return map[string]string{
		c.Password: "[password]",
	}
}

func (c *mongoDBConnectionProducer) getConnectionURL() string {
	return dbutil.QueryHelper(c.ConnectionURL, map[string]string{
		"username": c.Username,
		"password": c.Password,
	})
}

func (c *mongoDBConnectionProducer) makeClientOpts() (*options.ClientOptions, error) {
	writeOpts, err := c.getWriteConcern()
	if err != nil {
		return nil, err
	}

	authOpts, err := c.getTLSAuth()
	if err != nil {
		return nil, err
	}

	timeoutOpts, err := c.timeoutOpts()
	if err != nil {
		return nil, err
	}

	return options.MergeClientOptions(writeOpts, authOpts, timeoutOpts), nil
}

func (c *mongoDBConnectionProducer) getWriteConcern() (*options.ClientOptions, error) {
	if c.WriteConcern == "" {
		return nil, nil
	}

	input := c.WriteConcern

	// Operators sometimes paste the JSON encoded with base64 (e.g. through a
	// CI secret store that can't carry literal braces). Try decoding first;
	// if that fails treat the value as raw JSON.
	if decoded, err := base64.StdEncoding.DecodeString(input); err == nil {
		input = string(decoded)
	}

	concern := &writeConcernConfig{}
	if err := json.Unmarshal([]byte(input), concern); err != nil {
		return nil, fmt.Errorf("error unmarshalling write_concern: %w", err)
	}

	wc := &writeconcern.WriteConcern{
		WTimeout: time.Duration(concern.WTimeout) * time.Millisecond,
	}
	switch {
	case concern.W != 0:
		wc.W = concern.W
	case concern.WMode != "":
		wc.W = concern.WMode
	default:
		wc.W = "majority"
	}
	journal := concern.FSync || concern.J
	wc.Journal = &journal

	opts := options.Client()
	opts.SetWriteConcern(wc)
	return opts, nil
}

func (c *mongoDBConnectionProducer) getTLSAuth() (*options.ClientOptions, error) {
	if len(c.TLSCAData) == 0 && len(c.TLSCertificateKeyData) == 0 {
		return nil, nil
	}

	opts := options.Client()
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}

	if len(c.TLSCAData) > 0 {
		tlsConfig.RootCAs = x509.NewCertPool()
		if !tlsConfig.RootCAs.AppendCertsFromPEM(c.TLSCAData) {
			return nil, errors.New("failed to append CA to client options")
		}
	}

	if len(c.TLSCertificateKeyData) > 0 {
		certificate, err := tls.X509KeyPair(c.TLSCertificateKeyData, c.TLSCertificateKeyData)
		if err != nil {
			return nil, fmt.Errorf("unable to load tls_certificate_key_data: %w", err)
		}

		opts.SetAuth(options.Credential{
			AuthMechanism: "MONGODB-X509",
			Username:      c.Username,
		})

		tlsConfig.Certificates = append(tlsConfig.Certificates, certificate)
	}

	opts.SetTLSConfig(tlsConfig)
	return opts, nil
}

func (c *mongoDBConnectionProducer) timeoutOpts() (*options.ClientOptions, error) {
	opts := options.Client()

	if c.SocketTimeout < 0 {
		return nil, errors.New("socket_timeout must be >= 0")
	}

	if c.SocketTimeout == 0 {
		opts.SetSocketTimeout(1 * time.Minute)
	} else {
		opts.SetSocketTimeout(c.SocketTimeout)
	}

	if c.ConnectTimeout == 0 {
		opts.SetConnectTimeout(1 * time.Minute)
	} else {
		opts.SetConnectTimeout(c.ConnectTimeout)
	}

	if c.ServerSelectionTimeout != 0 {
		opts.SetServerSelectionTimeout(c.ServerSelectionTimeout)
	}

	return opts, nil
}
