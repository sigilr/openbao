// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package mssql

import (
	"context"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	mssql "github.com/microsoft/go-mssqldb"
	"github.com/microsoft/go-mssqldb/msdsn"

	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-secure-stdlib/strutil"

	metrics "github.com/hashicorp/go-metrics/compat"
	"github.com/openbao/openbao/sdk/v2/physical"
)

// Verify MSSQLBackend satisfies the correct interfaces
var _ physical.Backend = (*MSSQLBackend)(nil)

var identifierRegex = regexp.MustCompile(`^[\p{L}_][\p{L}\p{Nd}@#$_]*$`)

type MSSQLBackend struct {
	dbTable    string
	client     *sql.DB
	statements map[string]*sql.Stmt
	logger     log.Logger
	permitPool *physical.PermitPool
}

func isInvalidIdentifier(name string) bool {
	return !identifierRegex.MatchString(name)
}

func NewMSSQLBackend(conf map[string]string, logger log.Logger) (physical.Backend, error) {
	username := conf["username"]
	password := conf["password"]

	server, ok := conf["server"]
	if !ok || server == "" {
		return nil, fmt.Errorf("missing server")
	}

	port := conf["port"]

	maxParStr, ok := conf["max_parallel"]
	var maxParInt int
	var err error
	if ok {
		maxParInt, err = strconv.Atoi(maxParStr)
		if err != nil {
			return nil, fmt.Errorf("failed parsing max_parallel parameter: %w", err)
		}
		logger.Debug("max_parallel set", "max_parallel", maxParInt)
	} else {
		maxParInt = physical.DefaultParallelOperations
	}

	database, ok := conf["database"]
	if !ok {
		database = "openbao"
	}

	if isInvalidIdentifier(database) {
		return nil, fmt.Errorf("invalid database name")
	}

	table, ok := conf["table"]
	if !ok {
		table = "openbao"
	}

	if isInvalidIdentifier(table) {
		return nil, fmt.Errorf("invalid table name")
	}

	appname, ok := conf["appname"]
	if !ok {
		appname = "openbao"
	}

	connectionTimeout, ok := conf["connectiontimeout"]
	if !ok {
		connectionTimeout = "30"
	}

	logLevel, ok := conf["loglevel"]
	if !ok {
		logLevel = "0"
	}

	schema, ok := conf["schema"]
	if !ok || schema == "" {
		schema = "dbo"
	}

	if isInvalidIdentifier(schema) {
		return nil, fmt.Errorf("invalid schema name")
	}

	params := [][2]string{
		{"server", server},
		{"app name", appname},
		{"connection timeout", connectionTimeout},
		{"log", logLevel},
	}
	if username != "" {
		params = append(params, [2]string{"user id", username})
	}
	if password != "" {
		params = append(params, [2]string{"password", password})
	}
	if port != "" {
		params = append(params, [2]string{"port", port})
	}

	tlsParams, caFile, err := parseTLSConfig(conf)
	if err != nil {
		return nil, err
	}
	params = append(params, tlsParams...)

	cfg, err := msdsn.Parse(buildConnectionString(params))
	if err != nil {
		return nil, fmt.Errorf("failed to parse mssql connection parameters: %w", err)
	}

	if caFile != "" {
		if cfg.TLSConfig == nil {
			return nil, errors.New("tls_ca_file can't be used with encrypt = \"disable\"")
		}
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("unable to read tls_ca_file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("failed to parse tls_ca_file: no PEM certificates found")
		}
		cfg.TLSConfig.RootCAs = pool
	}

	db := sql.OpenDB(mssql.NewConnectorWithProcessQueryText(cfg))
	db.SetMaxOpenConns(maxParInt)

	if _, err := db.Exec("IF NOT EXISTS(SELECT * FROM sys.databases WHERE name = ?) CREATE DATABASE "+database, database); err != nil {
		return nil, fmt.Errorf("failed to create mssql database: %w", err)
	}

	dbTable := database + "." + schema + "." + table
	createQuery := "IF NOT EXISTS(SELECT 1 FROM " + database + ".INFORMATION_SCHEMA.TABLES WHERE TABLE_TYPE='BASE TABLE' AND TABLE_NAME=? AND TABLE_SCHEMA=?) CREATE TABLE " + dbTable + " (Path VARCHAR(512) PRIMARY KEY, Value VARBINARY(MAX))"

	if schema != "dbo" {
		var num int
		err = db.QueryRow("SELECT 1 FROM "+database+".sys.schemas WHERE name = ?", schema).Scan(&num)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := db.Exec("USE " + database + "; EXEC ('CREATE SCHEMA " + schema + "')"); err != nil {
				return nil, fmt.Errorf("failed to create mssql schema: %w", err)
			}

		case err != nil:
			return nil, fmt.Errorf("failed to check if mssql schema exists: %w", err)
		}
	}

	if _, err := db.Exec(createQuery, table, schema); err != nil {
		return nil, fmt.Errorf("failed to create mssql table: %w", err)
	}

	m := &MSSQLBackend{
		dbTable:    dbTable,
		client:     db,
		statements: make(map[string]*sql.Stmt),
		logger:     logger,
		permitPool: physical.NewPermitPool(maxParInt),
	}

	statements := map[string]string{
		"put": "IF EXISTS(SELECT 1 FROM " + dbTable + " WHERE Path = ?) UPDATE " + dbTable + " SET Value = ? WHERE Path = ?" +
			" ELSE INSERT INTO " + dbTable + " VALUES(?, ?)",
		"get":    "SELECT Value FROM " + dbTable + " WHERE Path = ?",
		"delete": "DELETE FROM " + dbTable + " WHERE Path = ?",
		"list":   "SELECT Path FROM " + dbTable + " WHERE Path LIKE ?",
	}

	for name, query := range statements {
		if err := m.prepare(name, query); err != nil {
			return nil, err
		}
	}

	return m, nil
}

func (m *MSSQLBackend) prepare(name, query string) error {
	stmt, err := m.client.Prepare(query)
	if err != nil {
		return fmt.Errorf("failed to prepare %q: %w", name, err)
	}

	m.statements[name] = stmt

	return nil
}

func (m *MSSQLBackend) Put(ctx context.Context, entry *physical.Entry) error {
	defer metrics.MeasureSince([]string{"mssql", "put"}, time.Now())

	m.permitPool.Acquire()
	defer m.permitPool.Release()

	_, err := m.statements["put"].ExecContext(ctx, entry.Key, entry.Value, entry.Key, entry.Key, entry.Value)
	return err
}

func (m *MSSQLBackend) Get(ctx context.Context, key string) (*physical.Entry, error) {
	defer metrics.MeasureSince([]string{"mssql", "get"}, time.Now())

	m.permitPool.Acquire()
	defer m.permitPool.Release()

	var result []byte
	err := m.statements["get"].QueryRowContext(ctx, key).Scan(&result)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	return &physical.Entry{
		Key:   key,
		Value: result,
	}, nil
}

func (m *MSSQLBackend) Delete(ctx context.Context, key string) error {
	defer metrics.MeasureSince([]string{"mssql", "delete"}, time.Now())

	m.permitPool.Acquire()
	defer m.permitPool.Release()

	_, err := m.statements["delete"].ExecContext(ctx, key)
	return err
}

// listAll returns every key stored directly under prefix (one path
// component, with a trailing slash for "folders"), sorted and
// deduplicated. It is shared by List and ListPage.
func (m *MSSQLBackend) listAll(ctx context.Context, prefix string) ([]string, error) {
	defer metrics.MeasureSince([]string{"mssql", "list"}, time.Now())

	m.permitPool.Acquire()
	defer m.permitPool.Release()

	likePrefix := prefix + "%"
	rows, err := m.statements["list"].QueryContext(ctx, likePrefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck

	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("failed to scan rows: %w", err)
		}

		key = strings.TrimPrefix(key, prefix)
		if i := strings.Index(key, "/"); i == -1 {
			keys = append(keys, key)
		} else {
			keys = strutil.AppendIfMissing(keys, key[:i+1])
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("failed to read rows: %w", err)
	}

	sort.Strings(keys)

	return keys, nil
}

// List is used to list all the keys under a given prefix, up to the next
// prefix.
func (m *MSSQLBackend) List(ctx context.Context, prefix string) ([]string, error) {
	return m.listAll(ctx, prefix)
}

// ListPage is used to list a page of keys under a given prefix, starting
// strictly after the "after" key, up to "limit" keys.
func (m *MSSQLBackend) ListPage(ctx context.Context, prefix, after string, limit int) ([]string, error) {
	keys, err := m.listAll(ctx, prefix)
	if err != nil {
		return nil, err
	}

	start := sort.SearchStrings(keys, after)
	for start < len(keys) && keys[start] <= after {
		start++
	}

	end := len(keys)
	if limit > 0 && start+limit < end {
		end = start + limit
	}

	return keys[start:end], nil
}

// parseTLSConfig maps the TLS options of the storage stanza to the driver's
// connection string keys, and returns the CA file to verify the server with.
//
// Without "encrypt" the driver only encrypts the login packet (unless the
// server forces encryption) and does not verify the server certificate, so
// "encrypt" must be set for the certificate to be checked.
func parseTLSConfig(conf map[string]string) ([][2]string, string, error) {
	var params [][2]string

	encrypt, encryptSet := conf["encrypt"]
	if encryptSet {
		encrypt = strings.ToLower(encrypt)
		switch encrypt {
		case "disable", "false", "true", "strict":
		default:
			return nil, "", fmt.Errorf("invalid encrypt %q: must be one of disable, false, true or strict", encrypt)
		}
		params = append(params, [2]string{"encrypt", encrypt})
	}

	if v, ok := conf["trust_server_certificate"]; ok {
		trust, err := strconv.ParseBool(v)
		if err != nil {
			return nil, "", fmt.Errorf("failed parsing trust_server_certificate parameter: %w", err)
		}
		params = append(params, [2]string{"trustservercertificate", strconv.FormatBool(trust)})
	}

	if v := conf["tls_server_name"]; v != "" {
		params = append(params, [2]string{"hostnameincertificate", v})
	}

	if v := conf["tls_min_version"]; v != "" {
		version, ok := map[string]string{
			"tls10": "1.0",
			"tls11": "1.1",
			"tls12": "1.2",
			"tls13": "1.3",
		}[v]
		if !ok {
			return nil, "", fmt.Errorf("invalid tls_min_version %q: must be one of tls10, tls11, tls12 or tls13", v)
		}
		params = append(params, [2]string{"tlsmin", version})
	}

	caFile := conf["tls_ca_file"]

	if !encryptSet && (len(params) > 0 || caFile != "") {
		return nil, "", errors.New("encrypt must be set to use trust_server_certificate, tls_ca_file, tls_server_name or tls_min_version")
	}

	return params, caFile, nil
}

// buildConnectionString builds an ADO style connection string, quoting every
// value so that a ';' or '"' in, say, the password can't change the meaning of
// the string.
func buildConnectionString(params [][2]string) string {
	parts := make([]string, 0, len(params))
	for _, p := range params {
		parts = append(parts, p[0]+"=\""+strings.ReplaceAll(p[1], `"`, `""`)+"\"")
	}
	return strings.Join(parts, ";")
}
