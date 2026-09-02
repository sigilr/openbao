// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

// Package kafka implements an OpenBao v5 database plugin for Apache Kafka
// using the AdminClient API via franz-go. Dynamic credentials are SCRAM-
// SHA-256 (default) or SCRAM-SHA-512 users on the cluster, with ACLs
// granted per the role doc supplied in creation_statements.
package kafka

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/hashicorp/go-secure-stdlib/strutil"
	"github.com/mitchellh/mapstructure"
	dbplugin "github.com/openbao/openbao/sdk/v2/database/dbplugin/v5"
	"github.com/openbao/openbao/sdk/v2/database/helper/dbutil"
	"github.com/openbao/openbao/sdk/v2/helper/template"
	"github.com/openbao/openbao/sdk/v2/logical"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl"
	saslscram "github.com/twmb/franz-go/pkg/sasl/scram"
)

const (
	kafkaTypeName = "kafka"

	defaultUserNameTemplate = `{{ printf "v-%s-%s-%s" (.DisplayName | truncate 10) (.RoleName | truncate 10) (random 10) | replace "." "-" | truncate 64 }}`
)

// ReportedVersion is overridable at build time.
var ReportedVersion = ""

// Kafka implements dbplugin.Database using the franz-go AdminClient.
// creation_statements is a JSON role doc:
//
//	{
//	  "mechanism": "SCRAM-SHA-256",   // or SCRAM-SHA-512; default 256
//	  "iterations": 4096,             // SCRAM iteration count; default 4096
//	  "acls": [
//	    {"resource_type":"TOPIC","resource_name":"*","pattern_type":"LITERAL",
//	     "operation":"READ","permission":"ALLOW"}
//	  ]
//	}
type Kafka struct {
	mu sync.Mutex

	config           *kafkaConfig
	client           *kgo.Client
	admin            *kadm.Client
	usernameProducer template.StringTemplate
}

type kafkaConfig struct {
	Brokers []string `mapstructure:"brokers"`

	Mechanism string `mapstructure:"mechanism"` // SCRAM-SHA-256, SCRAM-SHA-512, PLAIN
	Username  string `mapstructure:"username"`
	Password  string `mapstructure:"password"`

	TLSCA     string `mapstructure:"tls_ca"`
	TLSCAPath string `mapstructure:"tls_ca_path"`
	TLSCert   string `mapstructure:"tls_certificate"`
	TLSKey    string `mapstructure:"tls_key"`
	Insecure  bool   `mapstructure:"insecure"`
	UseTLS    bool   `mapstructure:"use_tls"`
}

type kafkaACL struct {
	ResourceType string `json:"resource_type"` // TOPIC, GROUP, CLUSTER, TRANSACTIONAL_ID, DELEGATION_TOKEN
	ResourceName string `json:"resource_name"`
	PatternType  string `json:"pattern_type"` // LITERAL or PREFIXED
	Operation    string `json:"operation"`    // READ, WRITE, CREATE, DELETE, ALTER, DESCRIBE, ...
	Permission   string `json:"permission"`   // ALLOW or DENY
}

type kafkaStatement struct {
	Mechanism  string     `json:"mechanism"`
	Iterations int        `json:"iterations"`
	ACLs       []kafkaACL `json:"acls"`
}

var (
	_ dbplugin.Database       = (*Kafka)(nil)
	_ logical.PluginVersioner = (*Kafka)(nil)
)

func New() (any, error) {
	db := newKafka()
	return dbplugin.NewDatabaseErrorSanitizerMiddleware(db, db.secretValues), nil
}

func newKafka() *Kafka {
	return &Kafka{}
}

func (k *Kafka) secretValues() map[string]string {
	if k.config == nil {
		return map[string]string{}
	}
	return map[string]string{k.config.Password: "[password]"}
}

func (k *Kafka) Type() (string, error) {
	return kafkaTypeName, nil
}

func (k *Kafka) PluginVersion() logical.PluginVersion {
	return logical.PluginVersion{Version: ReportedVersion}
}

func (k *Kafka) Close() error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.client != nil {
		k.client.Close()
	}
	k.client = nil
	k.admin = nil
	return nil
}

func (k *Kafka) Initialize(ctx context.Context, req dbplugin.InitializeRequest) (dbplugin.InitializeResponse, error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	cfg := &kafkaConfig{}
	if err := mapstructure.WeakDecode(req.Config, cfg); err != nil {
		return dbplugin.InitializeResponse{}, err
	}
	if len(cfg.Brokers) == 0 {
		return dbplugin.InitializeResponse{}, errors.New("brokers is required")
	}
	if cfg.Mechanism == "" {
		cfg.Mechanism = "SCRAM-SHA-256"
	}

	mech, err := pickMechanism(cfg)
	if err != nil {
		return dbplugin.InitializeResponse{}, err
	}

	opts := []kgo.Opt{
		kgo.SeedBrokers(cfg.Brokers...),
		kgo.SASL(mech),
	}
	if cfg.UseTLS || cfg.TLSCA != "" || cfg.TLSCAPath != "" || cfg.TLSCert != "" {
		tlsCfg, err := buildTLS(cfg)
		if err != nil {
			return dbplugin.InitializeResponse{}, err
		}
		opts = append(opts, kgo.DialTLSConfig(tlsCfg))
	}

	client, err := kgo.NewClient(opts...)
	if err != nil {
		return dbplugin.InitializeResponse{}, fmt.Errorf("kafka client: %w", err)
	}
	admin := kadm.NewClient(client)

	usernameTemplate, err := strutil.GetString(req.Config, "username_template")
	if err != nil {
		client.Close()
		return dbplugin.InitializeResponse{}, err
	}
	if usernameTemplate == "" {
		usernameTemplate = defaultUserNameTemplate
	}
	up, err := template.NewTemplate(template.Template(usernameTemplate))
	if err != nil {
		client.Close()
		return dbplugin.InitializeResponse{}, fmt.Errorf("invalid username_template: %w", err)
	}
	if _, err := up.Generate(dbplugin.UsernameMetadata{}); err != nil {
		client.Close()
		return dbplugin.InitializeResponse{}, fmt.Errorf("invalid username template: %w", err)
	}

	k.config = cfg
	k.client = client
	k.admin = admin
	k.usernameProducer = up

	if req.VerifyConnection {
		if _, err := admin.ApiVersions(ctx); err != nil {
			client.Close()
			return dbplugin.InitializeResponse{}, fmt.Errorf("failed to verify connection: %w", err)
		}
	}

	return dbplugin.InitializeResponse{Config: req.Config}, nil
}

// NewUser creates a SCRAM credential and grants the ACLs from the statement.
// If ACL creation fails after the credential is created, the credential is
// deleted to avoid leaving a half-configured user.
func (k *Kafka) NewUser(ctx context.Context, req dbplugin.NewUserRequest) (dbplugin.NewUserResponse, error) {
	if len(req.Statements.Commands) == 0 {
		return dbplugin.NewUserResponse{}, dbutil.ErrEmptyCreationStatement
	}

	var stmt kafkaStatement
	if err := json.Unmarshal([]byte(req.Statements.Commands[0]), &stmt); err != nil {
		return dbplugin.NewUserResponse{}, fmt.Errorf("creation_statements must be a JSON role doc: %w", err)
	}
	if stmt.Mechanism == "" {
		stmt.Mechanism = "SCRAM-SHA-256"
	}
	if stmt.Iterations == 0 {
		stmt.Iterations = 4096
	}

	mech, err := kadmScramMechanism(stmt.Mechanism)
	if err != nil {
		return dbplugin.NewUserResponse{}, err
	}

	k.mu.Lock()
	defer k.mu.Unlock()

	username, err := k.usernameProducer.Generate(req.UsernameConfig)
	if err != nil {
		return dbplugin.NewUserResponse{}, err
	}

	upsert := kadm.UpsertSCRAM{
		User:       username,
		Mechanism:  mech,
		Iterations: int32(stmt.Iterations),
		Password:   req.Password,
	}
	if _, err := k.admin.AlterUserSCRAMs(ctx, nil, []kadm.UpsertSCRAM{upsert}); err != nil {
		return dbplugin.NewUserResponse{}, fmt.Errorf("create scram credential: %w", err)
	}

	cleanup := func(opErr error) (dbplugin.NewUserResponse, error) {
		// Clean up any created ACLs
		delAllow := kadm.NewACLs().AnyResource().Allow("User:" + username).AllowHosts().Operations(kadm.OpAny)
		_, _ = k.admin.DeleteACLs(ctx, delAllow)
		delDeny := kadm.NewACLs().AnyResource().Deny("User:" + username).DenyHosts().Operations(kadm.OpAny)
		_, _ = k.admin.DeleteACLs(ctx, delDeny)
		// Clean up created SCRAM credential
		_, _ = k.admin.AlterUserSCRAMs(ctx,
			[]kadm.DeleteSCRAM{{User: username, Mechanism: mech}}, nil)
		return dbplugin.NewUserResponse{}, opErr
	}

	if len(stmt.ACLs) > 0 {
		for _, acl := range stmt.ACLs {
			op, err := parseACLOperation(acl.Operation)
			if err != nil {
				return cleanup(err)
			}
			pattern, err := parseACLPattern(acl.PatternType)
			if err != nil {
				return cleanup(err)
			}

			b := kadm.NewACLs().ResourcePatternType(pattern).Operations(op)

			switch strings.ToUpper(strings.TrimSpace(acl.Permission)) {
			case "DENY":
				b.Deny("User:" + username)
			case "", "ALLOW":
				b.Allow("User:" + username)
			default:
				return cleanup(fmt.Errorf("unsupported ACL permission %q", acl.Permission))
			}

			resName := strings.TrimSpace(acl.ResourceName)
			switch strings.ToUpper(strings.TrimSpace(acl.ResourceType)) {
			case "TOPIC":
				if resName == "" {
					return cleanup(errors.New("resource_name is required for resource_type TOPIC"))
				}
				b.Topics(resName)
			case "GROUP":
				if resName == "" {
					return cleanup(errors.New("resource_name is required for resource_type GROUP"))
				}
				b.Groups(resName)
			case "CLUSTER":
				b.Clusters()
			case "TRANSACTIONAL_ID", "TRANSACTIONALID":
				if resName == "" {
					return cleanup(errors.New("resource_name is required for resource_type TRANSACTIONAL_ID"))
				}
				b.TransactionalIDs(resName)
			case "DELEGATION_TOKEN", "DELEGATIONTOKEN":
				if resName == "" {
					return cleanup(errors.New("resource_name is required for resource_type DELEGATION_TOKEN"))
				}
				b.DelegationTokens(resName)
			default:
				return cleanup(fmt.Errorf("unsupported ACL resource_type %q", acl.ResourceType))
			}

			results, err := k.admin.CreateACLs(ctx, b)
			if err != nil {
				return cleanup(fmt.Errorf("create ACL: %w", err))
			}
			for _, res := range results {
				if res.Err != nil {
					return cleanup(fmt.Errorf("create ACL failed on resource %s: %w", res.Name, res.Err))
				}
			}
		}
	}

	return dbplugin.NewUserResponse{Username: username}, nil
}

func (k *Kafka) UpdateUser(ctx context.Context, req dbplugin.UpdateUserRequest) (dbplugin.UpdateUserResponse, error) {
	if req.Username == "" {
		return dbplugin.UpdateUserResponse{}, errors.New("missing username")
	}
	if req.Password == nil && req.Expiration == nil {
		return dbplugin.UpdateUserResponse{}, errors.New("no changes requested")
	}
	if req.Password == nil {
		return dbplugin.UpdateUserResponse{}, nil
	}

	mechName := "SCRAM-SHA-256"
	if k.config != nil && (k.config.Mechanism == "SCRAM-SHA-512" || k.config.Mechanism == "SCRAM-SHA-256") {
		mechName = k.config.Mechanism
	}
	mech, err := kadmScramMechanism(mechName)
	if err != nil {
		return dbplugin.UpdateUserResponse{}, err
	}

	k.mu.Lock()
	defer k.mu.Unlock()

	upsert := kadm.UpsertSCRAM{
		User:       req.Username,
		Mechanism:  mech,
		Iterations: 4096,
		Password:   req.Password.NewPassword,
	}
	if _, err := k.admin.AlterUserSCRAMs(ctx, nil, []kadm.UpsertSCRAM{upsert}); err != nil {
		return dbplugin.UpdateUserResponse{}, fmt.Errorf("update scram credential: %w", err)
	}
	return dbplugin.UpdateUserResponse{}, nil
}

func (k *Kafka) DeleteUser(ctx context.Context, req dbplugin.DeleteUserRequest) (dbplugin.DeleteUserResponse, error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	// 1. Delete all ACLs (Allow and Deny) for this principal across all resources/operations/hosts
	delAllow := kadm.NewACLs().AnyResource().Allow("User:" + req.Username).AllowHosts().Operations(kadm.OpAny)
	_, _ = k.admin.DeleteACLs(ctx, delAllow)

	delDeny := kadm.NewACLs().AnyResource().Deny("User:" + req.Username).DenyHosts().Operations(kadm.OpAny)
	_, _ = k.admin.DeleteACLs(ctx, delDeny)

	// 2. Delete SCRAM credentials per mechanism individually (Kafka rejects duplicate user in a single request)
	_, err256 := k.admin.AlterUserSCRAMs(ctx, []kadm.DeleteSCRAM{{User: req.Username, Mechanism: kadm.ScramSha256}}, nil)
	_, err512 := k.admin.AlterUserSCRAMs(ctx, []kadm.DeleteSCRAM{{User: req.Username, Mechanism: kadm.ScramSha512}}, nil)
	if err256 != nil && err512 != nil {
		return dbplugin.DeleteUserResponse{}, fmt.Errorf("delete scram credential: %w", err256)
	}
	return dbplugin.DeleteUserResponse{}, nil
}

// --- helpers ---------------------------------------------------------------

func pickMechanism(cfg *kafkaConfig) (sasl.Mechanism, error) {
	switch cfg.Mechanism {
	case "SCRAM-SHA-256":
		return saslscram.Auth{User: cfg.Username, Pass: cfg.Password}.AsSha256Mechanism(), nil
	case "SCRAM-SHA-512":
		return saslscram.Auth{User: cfg.Username, Pass: cfg.Password}.AsSha512Mechanism(), nil
	case "PLAIN":
		return nil, errors.New("PLAIN mechanism is not supported by the SCRAM AdminClient flow; use SCRAM-SHA-256 or 512")
	default:
		return nil, fmt.Errorf("unknown mechanism: %s", cfg.Mechanism)
	}
}

func kadmScramMechanism(name string) (kadm.ScramMechanism, error) {
	switch strings.ToUpper(name) {
	case "SCRAM-SHA-256", "SHA-256", "SHA256":
		return kadm.ScramSha256, nil
	case "SCRAM-SHA-512", "SHA-512", "SHA512":
		return kadm.ScramSha512, nil
	default:
		return 0, fmt.Errorf("unsupported mechanism %q", name)
	}
}

func buildTLS(cfg *kafkaConfig) (*tls.Config, error) {
	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: cfg.Insecure,
	}
	if cfg.TLSCA != "" || cfg.TLSCAPath != "" {
		pool := x509.NewCertPool()
		if cfg.TLSCA != "" {
			if !pool.AppendCertsFromPEM([]byte(cfg.TLSCA)) {
				return nil, errors.New("failed to parse tls_ca PEM")
			}
		}
		if cfg.TLSCAPath != "" {
			pem, err := os.ReadFile(cfg.TLSCAPath)
			if err != nil {
				return nil, fmt.Errorf("read tls_ca_path: %w", err)
			}
			if !pool.AppendCertsFromPEM(pem) {
				return nil, errors.New("failed to parse tls_ca_path PEM")
			}
		}
		tlsCfg.RootCAs = pool
	}
	if cfg.TLSCert != "" && cfg.TLSKey != "" {
		cert, err := tls.X509KeyPair([]byte(cfg.TLSCert), []byte(cfg.TLSKey))
		if err != nil {
			return nil, fmt.Errorf("client cert/key: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	return tlsCfg, nil
}

func parseACLOperation(op string) (kadm.ACLOperation, error) {
	switch strings.ToUpper(strings.TrimSpace(op)) {
	case "READ":
		return kadm.OpRead, nil
	case "WRITE":
		return kadm.OpWrite, nil
	case "CREATE":
		return kadm.OpCreate, nil
	case "DELETE":
		return kadm.OpDelete, nil
	case "ALTER":
		return kadm.OpAlter, nil
	case "DESCRIBE":
		return kadm.OpDescribe, nil
	case "CLUSTER_ACTION", "CLUSTERACTION":
		return kadm.OpClusterAction, nil
	case "DESCRIBE_CONFIGS", "DESCRIBECONFIGS":
		return kadm.OpDescribeConfigs, nil
	case "ALTER_CONFIGS", "ALTERCONFIGS":
		return kadm.OpAlterConfigs, nil
	case "IDEMPOTENT_WRITE", "IDEMPOTENTWRITE":
		return kadm.OpIdempotentWrite, nil
	case "ALL":
		return kadm.OpAll, nil
	default:
		return 0, fmt.Errorf("unsupported ACL operation %q", op)
	}
}

func parseACLPattern(p string) (kadm.ACLPattern, error) {
	switch strings.ToUpper(strings.TrimSpace(p)) {
	case "", "LITERAL":
		return kadm.ACLPatternLiteral, nil
	case "PREFIXED":
		return kadm.ACLPatternPrefixed, nil
	default:
		return 0, fmt.Errorf("unsupported ACL pattern_type %q", p)
	}
}
