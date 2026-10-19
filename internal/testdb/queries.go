package testdb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"testing"
)

// ObserveQueries opens a separate read pool with a hook before each query.
// Hooks may inject errors or mutate through the original pool before the read.
func ObserveQueries(t *testing.T, db *sql.DB, dsn string, before func(context.Context, string, []driver.NamedValue) error) *sql.DB {
	t.Helper()
	observed := sql.OpenDB(&queryConnector{base: db.Driver(), dsn: dsn, before: before})
	observed.SetMaxOpenConns(1)
	t.Cleanup(func() { observed.Close() })
	return observed
}

type queryConnector struct {
	base   driver.Driver
	dsn    string
	before func(context.Context, string, []driver.NamedValue) error
}

func (c *queryConnector) Driver() driver.Driver { return c.base }

func (c *queryConnector) Connect(ctx context.Context) (driver.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var conn driver.Conn
	var err error
	if contextual, ok := c.base.(driver.DriverContext); ok {
		var connector driver.Connector
		connector, err = contextual.OpenConnector(c.dsn)
		if err == nil {
			conn, err = connector.Connect(ctx)
		}
	} else {
		conn, err = c.base.Open(c.dsn)
	}
	if err != nil {
		return nil, err
	}
	queryer, ok := conn.(driver.QueryerContext)
	if !ok {
		_ = conn.Close()
		return nil, fmt.Errorf("query observation requires QueryerContext, got %T", conn)
	}
	return &queryConn{Conn: conn, queryer: queryer, before: c.before}, nil
}

type queryConn struct {
	driver.Conn
	queryer driver.QueryerContext
	before  func(context.Context, string, []driver.NamedValue) error
}

func (c *queryConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if err := c.before(ctx, query, args); err != nil {
		return nil, err
	}
	return c.queryer.QueryContext(ctx, query, args)
}
