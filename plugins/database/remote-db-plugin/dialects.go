// Copyright (c) KubeVault Authors
// SPDX-License-Identifier: Apache-2.0

package remotedb

import (
	"fmt"
	"strings"
)

var PostgresDialect = Dialect{
	TypeName: "remote-postgres",
	BuildCmd: func(connURL, stmt string) string {
		return fmt.Sprintf("psql %s -c %s", shellQuote(connURL), shellQuote(stmt))
	},
	BuildVerifyCmd: func(connURL string) string {
		return fmt.Sprintf("psql %s -c %s", shellQuote(connURL), shellQuote("SELECT 1"))
	},
	DefaultNewUserStmts: []string{
		`CREATE ROLE "{{username}}" WITH LOGIN PASSWORD '{{password}}' VALID UNTIL '{{expiration}}';`,
	},
	DefaultUpdatePasswordStmts: []string{
		`ALTER ROLE "{{username}}" WITH PASSWORD '{{password}}';`,
	},
	DefaultUpdateExpirationStmts: []string{
		`ALTER ROLE "{{username}}" VALID UNTIL '{{expiration}}';`,
	},
	DefaultDeleteUserStmts: []string{
		`REVOKE ALL PRIVILEGES ON ALL TABLES IN SCHEMA public FROM "{{username}}"; DROP ROLE IF EXISTS "{{username}}";`,
	},
}

var MySQLDialect = Dialect{
	TypeName: "remote-mysql",
	BuildCmd: func(connURL, stmt string) string {
		user, pass, host, port, db := parseMySQLDSN(connURL)
		return fmt.Sprintf("mysql -u%s -p%s -h%s -P%s %s --skip-ssl --execute=%s",
			user, pass, host, port, db, shellQuote(stmt))
	},
	BuildVerifyCmd: func(connURL string) string {
		user, pass, host, port, db := parseMySQLDSN(connURL)
		return fmt.Sprintf("mysql -u%s -p%s -h%s -P%s %s --skip-ssl --execute=%s",
			user, pass, host, port, db, shellQuote("SELECT 1"))
	},
	DefaultNewUserStmts: []string{
		"CREATE USER '{{username}}'@'%' IDENTIFIED BY '{{password}}';",
		"GRANT SELECT, INSERT, UPDATE, DELETE ON *.* TO '{{username}}'@'%';",
	},
	DefaultUpdatePasswordStmts: []string{
		"ALTER USER '{{username}}'@'%' IDENTIFIED BY '{{password}}';",
	},
	DefaultUpdateExpirationStmts: nil,
	DefaultDeleteUserStmts: []string{
		"REVOKE ALL PRIVILEGES, GRANT OPTION FROM '{{username}}'@'%';",
		"DROP USER IF EXISTS '{{username}}'@'%';",
	},
}

var ValkeyDialect = Dialect{
	TypeName: "remote-valkey",
	BuildCmd: func(connURL, stmt string) string {
		return fmt.Sprintf("redis-cli --no-auth-warning -u %s %s", shellQuote(connURL), stmt)
	},
	BuildVerifyCmd: func(connURL string) string {
		return fmt.Sprintf("redis-cli --no-auth-warning -u %s PING", shellQuote(connURL))
	},
	DefaultNewUserStmts: []string{
		"ACL SETUSER {{username}} ON >{{password}} ~* +@read",
	},
	DefaultUpdatePasswordStmts: []string{
		"ACL SETUSER {{username}} RESETPASS >{{password}}",
	},
	DefaultUpdateExpirationStmts: nil,
	DefaultDeleteUserStmts: []string{
		"ACL DELUSER {{username}}",
	},
}

func parseMySQLDSN(dsn string) (user, pass, host, port, db string) {
	user, pass, host, port, db = "", "", "localhost", "3306", ""

	atIdx := strings.LastIndex(dsn, "@")
	if atIdx < 0 {
		return
	}

	credentials := dsn[:atIdx]
	rest := dsn[atIdx+1:]

	if colonIdx := strings.Index(credentials, ":"); colonIdx >= 0 {
		user, pass = credentials[:colonIdx], credentials[colonIdx+1:]
	} else {
		user = credentials
	}

	rest = strings.TrimPrefix(rest, "tcp(")
	rest = strings.TrimPrefix(rest, "tcp")

	if slashIdx := strings.Index(rest, "/"); slashIdx >= 0 {
		hostPort := strings.TrimRight(rest[:slashIdx], ")")
		db = rest[slashIdx+1:]
		if colonIdx := strings.LastIndex(hostPort, ":"); colonIdx >= 0 {
			host = hostPort[:colonIdx]
			port = hostPort[colonIdx+1:]
		} else {
			host = hostPort
		}
	}
	return
}
