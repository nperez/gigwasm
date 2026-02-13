//go:build js && wasm

// Package wasmsql provides a database/sql driver that works inside WASM by
// calling host-provided database operations via //go:wasmimport. The host
// implements the "wasmsql" namespace — a 6-function streaming database
// passthrough using binary TLV on the wire. Any SQL backend works on the
// host side; the guest doesn't know or care what's behind the pipe.
//
// Register this driver with a blank import:
//
//	import _ "nickandperla.net/gigwasm/wasmsql"
//
// Then use database/sql as usual:
//
//	db, _ := sql.Open("sqlite", "/path/to/db")
package wasmsql

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"math"
	"unsafe"
)

func init() {
	sql.Register("wasmsql", &Driver{})
}

// --- wasmimport declarations (6 functions in "wasmsql" namespace) ---

//go:wasmimport wasmsql open
func hostOpen(pathPtr unsafe.Pointer, pathLen int32) int32

//go:wasmimport wasmsql close
func hostClose(db int32) int32

//go:wasmimport wasmsql exec
func hostExec(db int32, sqlPtr unsafe.Pointer, sqlLen int32,
	paramsPtr unsafe.Pointer, paramsLen int32,
	resultPtr unsafe.Pointer, resultLen int32) int32

//go:wasmimport wasmsql query
func hostQuery(db int32, sqlPtr unsafe.Pointer, sqlLen int32,
	paramsPtr unsafe.Pointer, paramsLen int32,
	resultPtr unsafe.Pointer, resultLen int32) int32

//go:wasmimport wasmsql next
func hostNext(handle int32, rowPtr unsafe.Pointer, rowLen int32) int32

//go:wasmimport wasmsql close_rows
func hostCloseRows(handle int32) int32

// --- TLV type bytes ---

const (
	tlvNull    = 0x00
	tlvInt64   = 0x01
	tlvFloat64 = 0x02
	tlvText    = 0x03
	tlvBlob    = 0x04
	tlvBool    = 0x05
)

// --- Little-endian helpers (raw byte manipulation, no imports) ---

func leU32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

func leI64(b []byte) int64 {
	return int64(leU32(b)) | int64(leU32(b[4:]))<<32
}

func leF64(b []byte) float64 {
	return math.Float64frombits(uint64(leU32(b)) | uint64(leU32(b[4:]))<<32)
}

func putU32(b []byte, v uint32) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
}

func putI64(b []byte, v int64) {
	putU32(b, uint32(v))
	putU32(b[4:], uint32(v>>32))
}

func putF64(b []byte, v float64) {
	bits := math.Float64bits(v)
	putU32(b, uint32(bits))
	putU32(b[4:], uint32(bits>>32))
}

// --- TLV encoding (guest → host: params) ---

func encodeParams(args []driver.Value) []byte {
	if len(args) == 0 {
		return []byte{0, 0, 0, 0} // count = 0
	}
	// Estimate capacity: 4 (count) + ~10 bytes per param
	buf := make([]byte, 4, 4+len(args)*10)
	putU32(buf, uint32(len(args)))

	for _, arg := range args {
		switch v := arg.(type) {
		case nil:
			buf = append(buf, tlvNull)
		case int64:
			buf = append(buf, tlvInt64, 0, 0, 0, 0, 0, 0, 0, 0)
			putI64(buf[len(buf)-8:], v)
		case float64:
			buf = append(buf, tlvFloat64, 0, 0, 0, 0, 0, 0, 0, 0)
			putF64(buf[len(buf)-8:], v)
		case string:
			b := []byte(v)
			buf = append(buf, tlvText, 0, 0, 0, 0)
			putU32(buf[len(buf)-4:], uint32(len(b)))
			buf = append(buf, b...)
		case []byte:
			buf = append(buf, tlvBlob, 0, 0, 0, 0)
			putU32(buf[len(buf)-4:], uint32(len(v)))
			buf = append(buf, v...)
		case bool:
			buf = append(buf, tlvBool)
			if v {
				buf = append(buf, 1)
			} else {
				buf = append(buf, 0)
			}
		default:
			// Convert to string
			s := fmt.Sprintf("%v", v)
			b := []byte(s)
			buf = append(buf, tlvText, 0, 0, 0, 0)
			putU32(buf[len(buf)-4:], uint32(len(b)))
			buf = append(buf, b...)
		}
	}
	return buf
}

// --- TLV decoding (host → guest: results) ---

func decodeExecResult(b []byte) (lastInsertID, rowsAffected int64) {
	if len(b) < 16 {
		return 0, 0
	}
	return leI64(b[0:]), leI64(b[8:])
}

func decodeQueryHeader(b []byte) (handle int32, cols []string) {
	if len(b) < 8 {
		return 0, nil
	}
	handle = int32(leU32(b[0:]))
	numCols := leU32(b[4:])
	pos := 8
	cols = make([]string, numCols)
	for i := uint32(0); i < numCols; i++ {
		if pos+4 > len(b) {
			break
		}
		n := leU32(b[pos:])
		pos += 4
		if pos+int(n) > len(b) {
			break
		}
		cols[i] = string(b[pos : pos+int(n)])
		pos += int(n)
	}
	return handle, cols
}

func decodeRow(b []byte, numCols int, dest []driver.Value) {
	pos := 0
	for i := 0; i < numCols && pos < len(b); i++ {
		typ := b[pos]
		pos++
		switch typ {
		case tlvNull:
			dest[i] = nil
		case tlvInt64:
			dest[i] = leI64(b[pos:])
			pos += 8
		case tlvFloat64:
			dest[i] = leF64(b[pos:])
			pos += 8
		case tlvText:
			n := leU32(b[pos:])
			pos += 4
			dest[i] = string(b[pos : pos+int(n)])
			pos += int(n)
		case tlvBlob:
			n := leU32(b[pos:])
			pos += 4
			blob := make([]byte, n)
			copy(blob, b[pos:pos+int(n)])
			pos += int(n)
			dest[i] = blob
		case tlvBool:
			dest[i] = b[pos] != 0
			pos++
		default:
			dest[i] = nil
		}
	}
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
	return &Stmt{conn: c, query: query}, nil
}

func (c *Conn) Close() error {
	rc := hostClose(c.db)
	if rc != 0 {
		return fmt.Errorf("wasmsql: close failed")
	}
	return nil
}

func (c *Conn) Begin() (driver.Tx, error) {
	if err := c.execDirect("BEGIN"); err != nil {
		return nil, err
	}
	return &Tx{conn: c}, nil
}

func (c *Conn) execDirect(query string) error {
	stmt := &Stmt{conn: c, query: query}
	_, err := stmt.Exec(nil)
	return err
}

// --- Tx ---

type Tx struct {
	conn *Conn
}

func (tx *Tx) Commit() error {
	return tx.conn.execDirect("COMMIT")
}

func (tx *Tx) Rollback() error {
	return tx.conn.execDirect("ROLLBACK")
}

// --- Stmt ---

type Stmt struct {
	conn  *Conn
	query string
}

func (s *Stmt) Close() error {
	return nil // no-op: no host-side handle
}

func (s *Stmt) NumInput() int {
	return -1 // variable
}

func (s *Stmt) Exec(args []driver.Value) (driver.Result, error) {
	sqlBytes := []byte(s.query)
	paramBytes := encodeParams(args)

	var sqlPtr unsafe.Pointer
	if len(sqlBytes) > 0 {
		sqlPtr = unsafe.Pointer(&sqlBytes[0])
	}
	var paramsPtr unsafe.Pointer
	var paramsLen int32
	if len(paramBytes) > 0 {
		paramsPtr = unsafe.Pointer(&paramBytes[0])
		paramsLen = int32(len(paramBytes))
	}

	buf := make([]byte, 64)
	n := hostExec(s.conn.db,
		sqlPtr, int32(len(sqlBytes)),
		paramsPtr, paramsLen,
		unsafe.Pointer(&buf[0]), int32(len(buf)),
	)

	if n >= 0 {
		lastID, affected := decodeExecResult(buf[:n])
		return &Result{lastID: lastID, changes: affected}, nil
	}
	if n <= -2 {
		errLen := int(-n) - 2
		if errLen > len(buf) {
			errLen = len(buf)
		}
		return nil, fmt.Errorf("wasmsql: %s", string(buf[:errLen]))
	}
	// n == -1: buffer too small (shouldn't happen for exec, result is 16 bytes)
	return nil, fmt.Errorf("wasmsql: exec result buffer too small")
}

func (s *Stmt) Query(args []driver.Value) (driver.Rows, error) {
	sqlBytes := []byte(s.query)
	paramBytes := encodeParams(args)

	var sqlPtr unsafe.Pointer
	if len(sqlBytes) > 0 {
		sqlPtr = unsafe.Pointer(&sqlBytes[0])
	}
	var paramsPtr unsafe.Pointer
	var paramsLen int32
	if len(paramBytes) > 0 {
		paramsPtr = unsafe.Pointer(&paramBytes[0])
		paramsLen = int32(len(paramBytes))
	}

	bufSize := 4096
	for attempts := 0; attempts < 2; attempts++ {
		buf := make([]byte, bufSize)
		n := hostQuery(s.conn.db,
			sqlPtr, int32(len(sqlBytes)),
			paramsPtr, paramsLen,
			unsafe.Pointer(&buf[0]), int32(len(buf)),
		)

		if n >= 0 {
			handle, cols := decodeQueryHeader(buf[:n])
			return &Rows{
				conn:    s.conn,
				handle:  handle,
				cols:    cols,
				numCols: len(cols),
				buf:     make([]byte, 4096),
			}, nil
		}
		if n == -1 {
			required := leU32(buf[0:4])
			bufSize = int(required) + 64
			continue
		}
		// Error
		errLen := int(-n) - 2
		if errLen > len(buf) {
			errLen = len(buf)
		}
		return nil, fmt.Errorf("wasmsql: %s", string(buf[:errLen]))
	}
	return nil, fmt.Errorf("wasmsql: query header buffer retry exhausted")
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
	conn    *Conn
	handle  int32    // result handle from query()
	cols    []string // column names
	numCols int
	buf     []byte // reusable row buffer, grows as needed
}

func (r *Rows) Columns() []string {
	return r.cols
}

func (r *Rows) Close() error {
	hostCloseRows(r.handle)
	return nil
}

func (r *Rows) Next(dest []driver.Value) error {
	for attempts := 0; attempts < 2; attempts++ {
		n := hostNext(r.handle, unsafe.Pointer(&r.buf[0]), int32(len(r.buf)))

		if n > 0 {
			decodeRow(r.buf[:n], r.numCols, dest)
			return nil
		}
		if n == 0 {
			return io.EOF
		}
		if n == -1 {
			// Buffer too small; grow
			required := leU32(r.buf[0:4])
			r.buf = make([]byte, int(required)+64)
			continue
		}
		// Error
		errLen := int(-n) - 2
		if errLen > len(r.buf) {
			errLen = len(r.buf)
		}
		return fmt.Errorf("wasmsql: %s", string(r.buf[:errLen]))
	}
	return fmt.Errorf("wasmsql: row buffer retry exhausted")
}
