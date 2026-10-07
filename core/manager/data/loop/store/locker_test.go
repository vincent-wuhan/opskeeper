package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
)

type recordingLockerDriver struct {
	mu         sync.Mutex
	queries    []string
	connection int
}

func (d *recordingLockerDriver) Open(string) (driver.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.connection++
	return &recordingLockerConn{driver: d, id: d.connection}, nil
}

func (d *recordingLockerDriver) Connect(context.Context) (driver.Conn, error) {
	return d.Open("")
}

func (d *recordingLockerDriver) Driver() driver.Driver { return d }

func (d *recordingLockerDriver) record(id int, query string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.queries = append(d.queries, fmt.Sprintf("%d:%s", id, query))
}

type recordingLockerConn struct {
	driver *recordingLockerDriver
	id     int
}

func (c *recordingLockerConn) Prepare(string) (driver.Stmt, error) {
	return nil, fmt.Errorf("prepare is not supported")
}

func (c *recordingLockerConn) Close() error { return nil }

func (c *recordingLockerConn) Begin() (driver.Tx, error) {
	return nil, fmt.Errorf("transactions are not supported")
}

func (c *recordingLockerConn) QueryContext(_ context.Context, query string, _ []driver.NamedValue) (driver.Rows, error) {
	c.driver.record(c.id, query)
	value := driver.Value(int64(1))
	if strings.HasPrefix(query, "SELECT pg_") {
		value = true
	}
	return &lockerRows{value: value}, nil
}

type lockerRows struct {
	remaining bool
	value     driver.Value
}

func (r *lockerRows) Columns() []string { return []string{"result"} }

func (r *lockerRows) Close() error { return nil }

func (r *lockerRows) Next(dest []driver.Value) error {
	if r.remaining {
		return io.EOF
	}
	r.remaining = true
	dest[0] = r.value
	return nil
}

func TestPostgreSQLLockerDBUsesSameConnectionForGetAndRelease(t *testing.T) {
	testDriver := &recordingLockerDriver{}
	sql.Register("postgres-locker-same-connection-test", testDriver)
	db := sql.OpenDB(testDriver)
	defer db.Close()

	locker := NewPostgreSQLLockerDB(db)
	acquired, err := locker.GetLock(context.Background(), "loop:incident", 1)
	if err != nil {
		t.Fatalf("GetLock: %v", err)
	}
	if !acquired {
		t.Fatal("GetLock = false, want true")
	}
	if err := locker.ReleaseLock(context.Background(), "loop:incident"); err != nil {
		t.Fatalf("ReleaseLock: %v", err)
	}

	testDriver.mu.Lock()
	defer testDriver.mu.Unlock()
	if len(testDriver.queries) != 2 {
		t.Fatalf("queries = %v, want pg advisory lock and unlock", testDriver.queries)
	}
	if !strings.Contains(testDriver.queries[0], "pg_try_advisory_lock") {
		t.Fatalf("get query = %q, want pg_try_advisory_lock", testDriver.queries[0])
	}
	if !strings.Contains(testDriver.queries[1], "pg_advisory_unlock") {
		t.Fatalf("release query = %q, want pg_advisory_unlock", testDriver.queries[1])
	}
	if testDriver.queries[0][:1] != testDriver.queries[1][:1] {
		t.Fatalf("advisory lock connections differ: %v", testDriver.queries)
	}
}

func TestLockerDBUsesSameConnectionForGetAndRelease(t *testing.T) {
	testDriver := &recordingLockerDriver{}
	sql.Register("locker-same-connection-test", testDriver)
	db := sql.OpenDB(testDriver)
	defer db.Close()

	locker := NewLockerDB(db)
	acquired, err := locker.GetLock(context.Background(), "loop:incident", 5)
	if err != nil {
		t.Fatalf("GetLock: %v", err)
	}
	if !acquired {
		t.Fatal("GetLock = false, want true")
	}
	if err := locker.ReleaseLock(context.Background(), "loop:incident"); err != nil {
		t.Fatalf("ReleaseLock: %v", err)
	}

	testDriver.mu.Lock()
	defer testDriver.mu.Unlock()
	if len(testDriver.queries) != 2 {
		t.Fatalf("queries = %v, want GET_LOCK and RELEASE_LOCK", testDriver.queries)
	}
	getConnection := testDriver.queries[0][:1]
	releaseConnection := testDriver.queries[1][:1]
	if getConnection != releaseConnection {
		t.Fatalf("GET_LOCK connection %s != RELEASE_LOCK connection %s", getConnection, releaseConnection)
	}
}
