package mysql

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/fatih/structs"
	stdmysql "github.com/go-sql-driver/mysql"
	"github.com/hashicorp/vault/api"
	"github.com/hashicorp/vault/builtin/logical/database/dbplugin"
	"github.com/hashicorp/vault/helper/strutil"
	"github.com/hashicorp/vault/plugins"
	"github.com/hashicorp/vault/plugins/helper/database/connutil"
	"github.com/hashicorp/vault/plugins/helper/database/credsutil"
	"github.com/hashicorp/vault/plugins/helper/database/dbutil"
	"github.com/mitchellh/mapstructure"
)

const (
	defaultMysqlRevocationStmts = `
		REVOKE ALL PRIVILEGES, GRANT OPTION FROM '{{name}}'@'%'; 
		DROP USER '{{name}}'@'%'
	`

	defaultMySQLRotateRootCredentialsSQL = `
		ALTER USER '{{username}}'@'%' IDENTIFIED BY '{{password}}';
	`

	mySQLTypeName = "mysql"
)

var (
	MetadataLen       int = 10
	LegacyMetadataLen int = 4
	UsernameLen       int = 32
	LegacyUsernameLen int = 16
)

var _ dbplugin.Database = &MySQL{}

type MySQL struct {
	*connutil.SQLConnectionProducer
	credsutil.CredentialsProducer
}

// New implements builtinplugins.BuiltinFactory
func New(displayNameLen, roleNameLen, usernameLen int) func() (interface{}, error) {
	return func() (interface{}, error) {
		connProducer := &connutil.SQLConnectionProducer{}
		connProducer.Type = mySQLTypeName

		credsProducer := &credsutil.SQLCredentialsProducer{
			DisplayNameLen: displayNameLen,
			RoleNameLen:    roleNameLen,
			UsernameLen:    usernameLen,
			Separator:      "-",
		}

		dbType := &MySQL{
			SQLConnectionProducer: connProducer,
			CredentialsProducer:   credsProducer,
		}

		return dbType, nil
	}
}

// Run instantiates a MySQL object, and runs the RPC server for the plugin
func Run(apiTLSConfig *api.TLSConfig) error {
	return runCommon(false, apiTLSConfig)
}

// Run instantiates a MySQL object, and runs the RPC server for the plugin
func RunLegacy(apiTLSConfig *api.TLSConfig) error {
	return runCommon(true, apiTLSConfig)
}

func runCommon(legacy bool, apiTLSConfig *api.TLSConfig) error {
	var f func() (interface{}, error)
	if legacy {
		f = New(credsutil.NoneLength, LegacyMetadataLen, LegacyUsernameLen)
	} else {
		f = New(MetadataLen, MetadataLen, UsernameLen)
	}
	dbType, err := f()
	if err != nil {
		return err
	}

	plugins.Serve(dbType.(*MySQL), apiTLSConfig)

	return nil
}

func (m *MySQL) Type() (string, error) {
	return mySQLTypeName, nil
}

func (m *MySQL) getConnection(ctx context.Context) (*sql.DB, error) {
	db, err := m.Connection(ctx)
	if err != nil {
		return nil, err
	}

	return db.(*sql.DB), nil
}

func (m *MySQL) CreateUser(ctx context.Context, statements dbplugin.Statements, usernameConfig dbplugin.UsernameConfig, expiration time.Time) (username string, password string, err error) {
	// Grab the lock
	m.Lock()
	defer m.Unlock()

	// Get the connection
	db, err := m.getConnection(ctx)
	if err != nil {
		return "", "", err
	}

	if len(statements.CreationStatements) == 0 {
		return "", "", dbutil.ErrEmptyCreationStatement
	}

	username, err = m.GenerateUsername(usernameConfig)
	if err != nil {
		return "", "", err
	}

	password, err = m.GeneratePassword()
	if err != nil {
		return "", "", err
	}

	expirationStr, err := m.GenerateExpiration(expiration)
	if err != nil {
		return "", "", err
	}

	// Start a transaction
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback()

	// Execute each query
	for _, stmt := range statements.CreationStatements {
		for _, query := range strutil.ParseArbitraryStringSlice(stmt, ";") {
			query = strings.TrimSpace(query)
			if len(query) == 0 {
				continue
			}
			query = dbutil.QueryHelper(query, map[string]string{
				"name":       username,
				"password":   password,
				"expiration": expirationStr,
			})

			stmt, err := tx.PrepareContext(ctx, query)
			if err != nil {
				// If the error code we get back is Error 1295: This command is not
				// supported in the prepared statement protocol yet, we will execute
				// the statement without preparing it. This allows the caller to
				// manually prepare statements, as well as run other not yet
				// prepare supported commands. If there is no error when running we
				// will continue to the next statement.
				if e, ok := err.(*stdmysql.MySQLError); ok && e.Number == 1295 {
					_, err = tx.ExecContext(ctx, query)
					if err != nil {
						return "", "", err
					}
					continue
				}

				return "", "", err
			}
			defer stmt.Close()
			if _, err := stmt.ExecContext(ctx); err != nil {
				return "", "", err
			}
		}
	}

	// Commit the transaction
	if err := tx.Commit(); err != nil {
		return "", "", err
	}

	return username, password, nil
}

// NOOP
func (m *MySQL) RenewUser(ctx context.Context, statements dbplugin.Statements, username string, expiration time.Time) error {
	return nil
}

func (m *MySQL) RevokeUser(ctx context.Context, statements dbplugin.Statements, username string) error {
	// Grab the read lock
	m.Lock()
	defer m.Unlock()

	// Get the connection
	db, err := m.getConnection(ctx)
	if err != nil {
		return err
	}

	revocationStmts := statements.RevocationStatements
	// Use a default SQL statement for revocation if one cannot be fetched from the role
	if len(revocationStmts) == 0 {
		revocationStmts = []string{defaultMysqlRevocationStmts}
	}

	// Start a transaction
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for _, stmt := range revocationStmts {
		for _, query := range strutil.ParseArbitraryStringSlice(stmt, ";") {
			query = strings.TrimSpace(query)
			if len(query) == 0 {
				continue
			}

			// This is not a prepared statement because not all commands are supported
			// 1295: This command is not supported in the prepared statement protocol yet
			// Reference https://mariadb.com/kb/en/mariadb/prepare-statement/
			query = strings.Replace(query, "{{name}}", username, -1)
			_, err = tx.ExecContext(ctx, query)
			if err != nil {
				return err
			}
		}
	}

	// Commit the transaction
	if err := tx.Commit(); err != nil {
		return err
	}

	return nil
}

func (p *MySQL) RotateRootCredentials(ctx context.Context, statements []string) (map[string]interface{}, error) {
	p.Lock()
	defer p.Unlock()

	c := new(connutil.SQLConfig)
	err := mapstructure.WeakDecode(p.SQLConnectionProducer.SQLConfig, c)
	if err != nil {
		return nil, err
	}

	if len(c.Username) == 0 || len(c.Password) == 0 {
		return nil, errors.New("username and password are required to rotate")
	}

	rotateStatents := statements
	if len(rotateStatents) == 0 {
		rotateStatents = []string{defaultMySQLRotateRootCredentialsSQL}
	}

	db, err := p.getConnection(ctx)
	if err != nil {
		return nil, err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() {
		tx.Rollback()
	}()

	password, err := p.GeneratePassword()
	if err != nil {
		return nil, err
	}

	for _, stmt := range rotateStatents {
		for _, query := range strutil.ParseArbitraryStringSlice(stmt, ";") {
			query = strings.TrimSpace(query)
			if len(query) == 0 {
				continue
			}
			stmt, err := tx.PrepareContext(ctx, dbutil.QueryHelper(query, map[string]string{
				"username": c.Username,
				"password": password,
			}))
			if err != nil {
				return nil, err
			}

			defer stmt.Close()
			if _, err := stmt.ExecContext(ctx); err != nil {
				return nil, err
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	if err := db.Close(); err != nil {
		return nil, err
	}

	c.Password = password
	return structs.Map(p.SQLConnectionProducer.SQLConfig), nil
}
