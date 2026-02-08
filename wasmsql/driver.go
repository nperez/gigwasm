//go:build js && wasm

// Package wasmsql provides a database/sql driver for SQLite that works inside
// WASM by calling host-provided SQLite C API functions via //go:wasmimport.
// It registers itself as the "sqlite" driver (matching modernc.org/sqlite).
package wasmsql

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"strconv"
	"unsafe"
)

func init() {
	sql.Register("sqlite", &Driver{})
}

// --- wasmimport declarations ---
// These call into the host "sqlite3" namespace.

//go:wasmimport sqlite3 open
func hostOpen(pathPtr unsafe.Pointer, pathLen int32) int32

//go:wasmimport sqlite3 close
func hostClose(db int32) int32

//go:wasmimport sqlite3 errmsg
func hostErrmsg(db int32, bufPtr unsafe.Pointer, bufLen int32) int32

//go:wasmimport sqlite3 prepare
func hostPrepare(db int32, sqlPtr unsafe.Pointer, sqlLen int32) int32

//go:wasmimport sqlite3 finalize
func hostFinalize(stmt int32) int32

//go:wasmimport sqlite3 reset
func hostReset(stmt int32) int32

//go:wasmimport sqlite3 bind_text
func hostBindText(stmt int32, idx int32, ptr unsafe.Pointer, length int32) int32

//go:wasmimport sqlite3 bind_int64
func hostBindInt64(stmt int32, idx int32, val int64) int32

//go:wasmimport sqlite3 bind_double
func hostBindDouble(stmt int32, idx int32, val float64) int32

//go:wasmimport sqlite3 bind_null
func hostBindNull(stmt int32, idx int32) int32

//go:wasmimport sqlite3 step
func hostStep(stmt int32) int32

//go:wasmimport sqlite3 column_count
func hostColumnCount(stmt int32) int32

//go:wasmimport sqlite3 column_type
func hostColumnType(stmt int32, col int32) int32

//go:wasmimport sqlite3 column_text
func hostColumnText(stmt int32, col int32, bufPtr unsafe.Pointer, bufLen int32) int32

//go:wasmimport sqlite3 column_int64
func hostColumnInt64(stmt int32, col int32) int64

//go:wasmimport sqlite3 column_double
func hostColumnDouble(stmt int32, col int32) float64

//go:wasmimport sqlite3 column_name
func hostColumnName(stmt int32, col int32, bufPtr unsafe.Pointer, bufLen int32) int32

//go:wasmimport sqlite3 last_insert_rowid
func hostLastInsertRowid(db int32) int64

//go:wasmimport sqlite3 changes
func hostChanges(db int32) int32

// SQLite result codes
const (
	sqliteOK   = 0
	sqliteRow  = 100
	sqliteDone = 101

	sqliteInteger = 1
	sqliteFloat   = 2
	sqliteText    = 3
	sqliteBlob    = 4
	sqliteNull    = 5
)

func getErrmsg(db int32) string {
	buf := make([]byte, 512)
	n := hostErrmsg(db, unsafe.Pointer(&buf[0]), int32(len(buf)))
	return string(buf[:n])
}

// --- Driver ---

type Driver struct{}

func (d *Driver) Open(name string) (driver.Conn, error) {
	nameBytes := []byte(name)
	handle := hostOpen(unsafe.Pointer(&nameBytes[0]), int32(len(nameBytes)))
	if handle == 0 {
		return nil, fmt.Errorf("wasmsql: failed to open database %q", name)
	}
	return &Conn{db: handle}, nil
}

// --- Conn ---

type Conn struct {
	db int32
}

func (c *Conn) Prepare(query string) (driver.Stmt, error) {
	qBytes := []byte(query)
	handle := hostPrepare(c.db, unsafe.Pointer(&qBytes[0]), int32(len(qBytes)))
	if handle == 0 {
		return nil, fmt.Errorf("wasmsql: prepare failed: %s", getErrmsg(c.db))
	}
	return &Stmt{conn: c, handle: handle, query: query}, nil
}

func (c *Conn) Close() error {
	rc := hostClose(c.db)
	if rc != sqliteOK {
		return fmt.Errorf("wasmsql: close failed")
	}
	return nil
}

func (c *Conn) Begin() (driver.Tx, error) {
	_, err := c.execDirect("BEGIN")
	if err != nil {
		return nil, err
	}
	return &Tx{conn: c}, nil
}

func (c *Conn) execDirect(query string) (driver.Result, error) {
	stmt, err := c.Prepare(query)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	return stmt.Exec(nil)
}

// --- Tx ---

type Tx struct {
	conn *Conn
}

func (tx *Tx) Commit() error {
	_, err := tx.conn.execDirect("COMMIT")
	return err
}

func (tx *Tx) Rollback() error {
	_, err := tx.conn.execDirect("ROLLBACK")
	return err
}

// --- Stmt ---

type Stmt struct {
	conn   *Conn
	handle int32
	query  string
}

func (s *Stmt) Close() error {
	rc := hostFinalize(s.handle)
	if rc != sqliteOK {
		return fmt.Errorf("wasmsql: finalize failed")
	}
	return nil
}

func (s *Stmt) NumInput() int {
	return -1 // variable number of inputs
}

func (s *Stmt) bindArgs(args []driver.Value) error {
	for i, arg := range args {
		idx := int32(i + 1) // 1-based
		switch v := arg.(type) {
		case nil:
			if rc := hostBindNull(s.handle, idx); rc != sqliteOK {
				return fmt.Errorf("wasmsql: bind_null failed")
			}
		case int64:
			if rc := hostBindInt64(s.handle, idx, v); rc != sqliteOK {
				return fmt.Errorf("wasmsql: bind_int64 failed")
			}
		case float64:
			if rc := hostBindDouble(s.handle, idx, v); rc != sqliteOK {
				return fmt.Errorf("wasmsql: bind_double failed")
			}
		case bool:
			var iv int64
			if v {
				iv = 1
			}
			if rc := hostBindInt64(s.handle, idx, iv); rc != sqliteOK {
				return fmt.Errorf("wasmsql: bind_int64 (bool) failed")
			}
		case string:
			b := []byte(v)
			var ptr unsafe.Pointer
			if len(b) > 0 {
				ptr = unsafe.Pointer(&b[0])
			} else {
				// Empty string: pass a non-nil pointer to avoid issues
				var zero byte
				ptr = unsafe.Pointer(&zero)
			}
			if rc := hostBindText(s.handle, idx, ptr, int32(len(b))); rc != sqliteOK {
				return fmt.Errorf("wasmsql: bind_text failed")
			}
		case []byte:
			// Bind as text (SQLite treats TEXT and BLOB similarly for many uses)
			var ptr unsafe.Pointer
			if len(v) > 0 {
				ptr = unsafe.Pointer(&v[0])
			} else {
				var zero byte
				ptr = unsafe.Pointer(&zero)
			}
			if rc := hostBindText(s.handle, idx, ptr, int32(len(v))); rc != sqliteOK {
				return fmt.Errorf("wasmsql: bind_text (bytes) failed")
			}
		default:
			// Convert to string
			str := fmt.Sprintf("%v", v)
			b := []byte(str)
			if rc := hostBindText(s.handle, idx, unsafe.Pointer(&b[0]), int32(len(b))); rc != sqliteOK {
				return fmt.Errorf("wasmsql: bind_text (default) failed")
			}
		}
	}
	return nil
}

func (s *Stmt) Exec(args []driver.Value) (driver.Result, error) {
	hostReset(s.handle)

	if err := s.bindArgs(args); err != nil {
		return nil, err
	}

	rc := hostStep(s.handle)
	if rc != sqliteDone && rc != sqliteRow {
		return nil, fmt.Errorf("wasmsql: exec step failed: %s", getErrmsg(s.conn.db))
	}

	// Drain any remaining rows
	for rc == sqliteRow {
		rc = hostStep(s.handle)
	}

	rowid := hostLastInsertRowid(s.conn.db)
	changes := int64(hostChanges(s.conn.db))
	return &Result{lastID: rowid, changes: changes}, nil
}

func (s *Stmt) Query(args []driver.Value) (driver.Rows, error) {
	hostReset(s.handle)

	if err := s.bindArgs(args); err != nil {
		return nil, err
	}

	// Step first to trigger query execution on the host, which
	// populates column metadata. Before this, column_count returns 0.
	rc := hostStep(s.handle)
	if rc != sqliteRow && rc != sqliteDone {
		return nil, fmt.Errorf("wasmsql: query step failed: %s", getErrmsg(s.conn.db))
	}

	numCols := hostColumnCount(s.handle)
	cols := make([]string, numCols)
	buf := make([]byte, 256)
	for i := int32(0); i < numCols; i++ {
		n := hostColumnName(s.handle, i, unsafe.Pointer(&buf[0]), int32(len(buf)))
		cols[i] = string(buf[:n])
	}

	done := rc == sqliteDone
	return &Rows{stmt: s, cols: cols, numCols: numCols, done: done, firstStep: rc}, nil
}

// --- Result ---

type Result struct {
	lastID  int64
	changes int64
}

func (r *Result) LastInsertId() (int64, error) {
	return r.lastID, nil
}

func (r *Result) RowsAffected() (int64, error) {
	return r.changes, nil
}

// --- Rows ---

type Rows struct {
	stmt      *Stmt
	cols      []string
	numCols   int32
	done      bool
	firstStep int32 // buffered step result from Query(); -1 = consumed
}

func (r *Rows) Columns() []string {
	return r.cols
}

func (r *Rows) Close() error {
	// Don't finalize — the Stmt.Close() handles that.
	// Just drain remaining rows.
	for !r.done {
		rc := hostStep(r.stmt.handle)
		if rc != sqliteRow {
			r.done = true
		}
	}
	return nil
}

func (r *Rows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}

	var rc int32
	if r.firstStep >= 0 {
		rc = r.firstStep
		r.firstStep = -1
	} else {
		rc = hostStep(r.stmt.handle)
	}

	if rc == sqliteDone {
		r.done = true
		return io.EOF
	}
	if rc != sqliteRow {
		return fmt.Errorf("wasmsql: step failed: %s", getErrmsg(r.stmt.conn.db))
	}

	buf := make([]byte, 4096)
	for i := int32(0); i < r.numCols; i++ {
		colType := hostColumnType(r.stmt.handle, i)
		switch colType {
		case sqliteNull:
			dest[i] = nil
		case sqliteInteger:
			dest[i] = hostColumnInt64(r.stmt.handle, i)
		case sqliteFloat:
			dest[i] = hostColumnDouble(r.stmt.handle, i)
		case sqliteText, sqliteBlob:
			n := hostColumnText(r.stmt.handle, i, unsafe.Pointer(&buf[0]), int32(len(buf)))
			dest[i] = string(buf[:n])
		default:
			n := hostColumnText(r.stmt.handle, i, unsafe.Pointer(&buf[0]), int32(len(buf)))
			s := string(buf[:n])
			// Try to parse as int64
			if v, err := strconv.ParseInt(s, 10, 64); err == nil {
				dest[i] = v
			} else {
				dest[i] = s
			}
		}
	}

	return nil
}
