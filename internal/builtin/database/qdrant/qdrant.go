// Package qdrant implements an OpenBao v5 database plugin for the
// Qdrant vector database.
//
// Qdrant supports granular access control (RBAC) via JSON Web Tokens (JWT)
// signed with the instance's admin API key using HS256:
//   - Initialize parses config and verifies the API key against `/readyz`.
//   - NewUser generates a unique username and signs an HS256 JWT containing
//     the permissions defined in the role's creation_statements and lease TTL (exp),
//     returning the signed JWT in NewUserResponse.Password.
//   - UpdateUser handles static-role rotation.
//   - DeleteUser is a no-op (Qdrant JWT tokens are stateless and expire at exp).
package qdrant

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v4"
	"github.com/mitchellh/mapstructure"
	dbplugin "github.com/openbao/openbao/sdk/v2/database/dbplugin/v5"
	"github.com/openbao/openbao/sdk/v2/helper/template"
	"github.com/openbao/openbao/sdk/v2/logical"
)

const (
	qdrantTypeName          = "qdrant"
	defaultUserNameTemplate = `{{ printf "v-%s-%s-%s-%s" (.DisplayName | truncate 8) (.RoleName | truncate 8) (random 20) (unix_time) | truncate 63 }}`
)

// ReportedVersion is overridable at build time.
var ReportedVersion = ""

// Qdrant implements dbplugin.Database for Qdrant.
type Qdrant struct {
	mu               sync.Mutex
	config           *qdrantConfig
	client           *http.Client
	usernameProducer template.StringTemplate
}

type qdrantConfig struct {
	URL    string `mapstructure:"url"`
	APIKey string `mapstructure:"api_key"`

	CACert     string `mapstructure:"ca_cert"`
	CAPath     string `mapstructure:"ca_path"`
	ClientCert string `mapstructure:"client_cert"`
	ClientKey  string `mapstructure:"client_key"`
	Insecure   bool   `mapstructure:"insecure"`
}

var (
	_ dbplugin.Database       = (*Qdrant)(nil)
	_ logical.PluginVersioner = (*Qdrant)(nil)
)

func New() (any, error) {
	db := newQdrant()
	return dbplugin.NewDatabaseErrorSanitizerMiddleware(db, db.secretValues), nil
}

func newQdrant() *Qdrant {
	up, _ := template.NewTemplate(template.Template(defaultUserNameTemplate))
	return &Qdrant{
		usernameProducer: up,
	}
}

func (q *Qdrant) secretValues() map[string]string {
	if q.config == nil {
		return map[string]string{}
	}
	return map[string]string{q.config.APIKey: "[api_key]"}
}

func (q *Qdrant) Type() (string, error) {
	return qdrantTypeName, nil
}

func (q *Qdrant) PluginVersion() logical.PluginVersion {
	return logical.PluginVersion{Version: ReportedVersion}
}

func (q *Qdrant) Close() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.client != nil {
		q.client.CloseIdleConnections()
	}
	q.client = nil
	return nil
}

func (q *Qdrant) Initialize(ctx context.Context, req dbplugin.InitializeRequest) (dbplugin.InitializeResponse, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	cfg := &qdrantConfig{}
	if err := mapstructure.WeakDecode(req.Config, cfg); err != nil {
		return dbplugin.InitializeResponse{}, err
	}
	if cfg.URL == "" {
		return dbplugin.InitializeResponse{}, errors.New("url is required")
	}

	client, err := newHTTPClient(cfg)
	if err != nil {
		return dbplugin.InitializeResponse{}, err
	}
	q.config = cfg
	q.client = client

	up, err := template.NewTemplate(template.Template(defaultUserNameTemplate))
	if err != nil {
		return dbplugin.InitializeResponse{}, fmt.Errorf("unable to initialize username template: %w", err)
	}
	q.usernameProducer = up

	if req.VerifyConnection {
		if err := q.healthcheck(ctx); err != nil {
			return dbplugin.InitializeResponse{}, fmt.Errorf("failed to verify connection: %w", err)
		}
	}

	return dbplugin.InitializeResponse{Config: req.Config}, nil
}

// NewUser generates a signed HS256 JWT token for Qdrant RBAC.
func (q *Qdrant) NewUser(ctx context.Context, req dbplugin.NewUserRequest) (dbplugin.NewUserResponse, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.config == nil || q.config.APIKey == "" {
		return dbplugin.NewUserResponse{}, errors.New("qdrant api_key is required in config to sign JWT tokens")
	}

	username, err := q.usernameProducer.Generate(req.UsernameConfig)
	if err != nil {
		return dbplugin.NewUserResponse{}, fmt.Errorf("failed to generate username: %w", err)
	}

	claims := jwt.MapClaims{
		"sub": username,
	}

	if !req.Expiration.IsZero() {
		claims["exp"] = req.Expiration.Unix()
	}

	// Parse creation statements for access permissions and claims.
	var collectionAccessList []any

	for _, cmd := range req.Statements.Commands {
		cmd = strings.TrimSpace(cmd)
		if cmd == "" {
			continue
		}

		// JSON Object
		if strings.HasPrefix(cmd, "{") {
			var obj map[string]any
			if err := json.Unmarshal([]byte(cmd), &obj); err == nil {
				// Single collection access rule: {"collection": "...", "access": "..."}
				if col, ok := obj["collection"]; ok && col != "" {
					collectionAccessList = append(collectionAccessList, obj)
					continue
				}

				for k, v := range obj {
					if k == "access" {
						if list, isList := v.([]any); isList {
							collectionAccessList = append(collectionAccessList, list...)
						} else {
							claims["access"] = v
						}
					} else {
						claims[k] = v
					}
				}
				continue
			}
		}

		// JSON Array
		if strings.HasPrefix(cmd, "[") {
			var arr []any
			if err := json.Unmarshal([]byte(cmd), &arr); err == nil {
				collectionAccessList = append(collectionAccessList, arr...)
				continue
			}
		}

		// Plain string shorthand
		switch strings.ToLower(cmd) {
		case "r", "read":
			claims["access"] = "r"
		case "m", "manage":
			claims["access"] = "m"
		default:
			// "col1:rw, col2:r"
			for _, part := range strings.Split(cmd, ",") {
				part = strings.TrimSpace(part)
				if part == "" {
					continue
				}
				if col, acc, found := strings.Cut(part, ":"); found {
					collectionAccessList = append(collectionAccessList, map[string]any{
						"collection": strings.TrimSpace(col),
						"access":     strings.TrimSpace(acc),
					})
				}
			}
		}
	}

	if len(collectionAccessList) > 0 {
		claims["access"] = collectionAccessList
	} else if _, hasAccess := claims["access"]; !hasAccess {
		// Default to manage ("m") if access claim is not set, matching Qdrant default
		claims["access"] = "m"
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signedToken, err := token.SignedString([]byte(q.config.APIKey))
	if err != nil {
		return dbplugin.NewUserResponse{}, fmt.Errorf("failed to sign JWT token: %w", err)
	}

	return dbplugin.NewUserResponse{
		Username: username,
		Password: signedToken,
	}, nil
}

// UpdateUser is a no-op against the server. Static-role rotation flows
// through this method and we want OpenBao to keep tracking the rotated
// value even though we can't push it to Qdrant.
func (q *Qdrant) UpdateUser(ctx context.Context, req dbplugin.UpdateUserRequest) (dbplugin.UpdateUserResponse, error) {
	if req.Username == "" {
		return dbplugin.UpdateUserResponse{}, errors.New("missing username")
	}
	if req.Password == nil && req.Expiration == nil {
		return dbplugin.UpdateUserResponse{}, errors.New("no changes requested")
	}
	return dbplugin.UpdateUserResponse{}, nil
}

// DeleteUser is a no-op because Qdrant JWT tokens are stateless and expire at exp.
func (q *Qdrant) DeleteUser(ctx context.Context, req dbplugin.DeleteUserRequest) (dbplugin.DeleteUserResponse, error) {
	return dbplugin.DeleteUserResponse{}, nil
}

func (q *Qdrant) healthcheck(ctx context.Context) error {
	base := strings.TrimRight(q.config.URL, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/readyz", nil)
	if err != nil {
		return err
	}
	if q.config.APIKey != "" {
		req.Header.Set("api-key", q.config.APIKey)
	}
	resp, err := q.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("qdrant /readyz failed: %s: %s", resp.Status, string(b))
	}
	return nil
}

func newHTTPClient(cfg *qdrantConfig) (*http.Client, error) {
	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: cfg.Insecure,
	}
	if cfg.CACert != "" || cfg.CAPath != "" {
		pool := x509.NewCertPool()
		if cfg.CACert != "" {
			if !pool.AppendCertsFromPEM([]byte(cfg.CACert)) {
				return nil, errors.New("failed to parse ca_cert PEM")
			}
		}
		if cfg.CAPath != "" {
			pem, err := os.ReadFile(cfg.CAPath)
			if err != nil {
				return nil, fmt.Errorf("read ca_path: %w", err)
			}
			if !pool.AppendCertsFromPEM(pem) {
				return nil, errors.New("failed to parse ca_path PEM")
			}
		}
		tlsCfg.RootCAs = pool
	}
	if cfg.ClientCert != "" && cfg.ClientKey != "" {
		cert, err := tls.X509KeyPair([]byte(cfg.ClientCert), []byte(cfg.ClientKey))
		if err != nil {
			return nil, fmt.Errorf("client cert/key: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
	}
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: tlsCfg},
	}, nil
}
