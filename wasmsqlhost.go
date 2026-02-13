package gigwasm

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"sync"

	_ "modernc.org/sqlite"

	"github.com/wasmerio/wasmer-go/wasmer"
)

// TLV type bytes for the wasmsql wire format.
const (
	tlvNull    = 0x00
	tlvInt64   = 0x01
	tlvFloat64 = 0x02
	tlvText    = 0x03
	tlvBlob    = 0x04
	tlvBool    = 0x05
)

type wasmsqlHost struct {
	mu         sync.Mutex
	driverName string
	dbs        map[int32]*sql.DB
	results    map[int32]*resultState
	nextID     int32
}

type resultState struct {
	rows    *sql.Rows
	cols    []string
	numCols int
	dbID    int32
	current []interface{} // scanned row values, nil = need to advance
	done    bool
}

func newWasmSQLHost(driverName string) *wasmsqlHost {
	return &wasmsqlHost{
		driverName: driverName,
		dbs:        make(map[int32]*sql.DB),
		results:    make(map[int32]*resultState),
		nextID:     1,
	}
}

func (h *wasmsqlHost) allocID() int32 {
	id := h.nextID
	h.nextID++
	return id
}

// writeError writes an error message to the result buffer and returns the
// negative return code per the wasmsql convention: -(errLen + 2).
func writeError(inst *GoInstance, resultPtr, resultLen int32, err error) int32 {
	msg := []byte(err.Error())
	if int32(len(msg)) > resultLen {
		msg = msg[:resultLen]
	}
	inst.WriteBytes(resultPtr, msg)
	return -int32(len(msg)) - 2
}

// --- TLV encoding helpers (host side) ---

func encodeExecResult(lastInsertID, rowsAffected int64) []byte {
	buf := make([]byte, 16)
	binary.LittleEndian.PutUint64(buf[0:], uint64(lastInsertID))
	binary.LittleEndian.PutUint64(buf[8:], uint64(rowsAffected))
	return buf
}

func encodeQueryHeader(handle int32, cols []string) []byte {
	var buf bytes.Buffer
	b := make([]byte, 4)

	// result handle
	binary.LittleEndian.PutUint32(b, uint32(handle))
	buf.Write(b)

	// numCols
	binary.LittleEndian.PutUint32(b, uint32(len(cols)))
	buf.Write(b)

	// column names
	for _, col := range cols {
		cb := []byte(col)
		binary.LittleEndian.PutUint32(b, uint32(len(cb)))
		buf.Write(b)
		buf.Write(cb)
	}

	return buf.Bytes()
}

func encodeRow(vals []interface{}) []byte {
	var buf bytes.Buffer
	for _, v := range vals {
		encodeValue(&buf, v)
	}
	return buf.Bytes()
}

func encodeValue(buf *bytes.Buffer, v interface{}) {
	b := make([]byte, 8)
	switch tv := v.(type) {
	case nil:
		buf.WriteByte(tlvNull)
	case int64:
		buf.WriteByte(tlvInt64)
		binary.LittleEndian.PutUint64(b, uint64(tv))
		buf.Write(b)
	case float64:
		buf.WriteByte(tlvFloat64)
		binary.LittleEndian.PutUint64(b, math.Float64bits(tv))
		buf.Write(b)
	case string:
		buf.WriteByte(tlvText)
		sb := []byte(tv)
		binary.LittleEndian.PutUint32(b, uint32(len(sb)))
		buf.Write(b[:4])
		buf.Write(sb)
	case []byte:
		buf.WriteByte(tlvBlob)
		binary.LittleEndian.PutUint32(b, uint32(len(tv)))
		buf.Write(b[:4])
		buf.Write(tv)
	case bool:
		buf.WriteByte(tlvBool)
		if tv {
			buf.WriteByte(1)
		} else {
			buf.WriteByte(0)
		}
	default:
		// Fall back to text representation
		buf.WriteByte(tlvText)
		sb := []byte(fmt.Sprintf("%v", tv))
		binary.LittleEndian.PutUint32(b, uint32(len(sb)))
		buf.Write(b[:4])
		buf.Write(sb)
	}
}

// --- TLV decoding helpers (host side, for params) ---

func decodeParams(data []byte) ([]interface{}, error) {
	if len(data) < 4 {
		return nil, nil
	}
	count := binary.LittleEndian.Uint32(data[0:4])
	pos := 4
	params := make([]interface{}, count)
	for i := uint32(0); i < count; i++ {
		if pos >= len(data) {
			return nil, fmt.Errorf("wasmsql: truncated params at index %d", i)
		}
		typ := data[pos]
		pos++
		switch typ {
		case tlvNull:
			params[i] = nil
		case tlvInt64:
			if pos+8 > len(data) {
				return nil, fmt.Errorf("wasmsql: truncated int64 param")
			}
			params[i] = int64(binary.LittleEndian.Uint64(data[pos:]))
			pos += 8
		case tlvFloat64:
			if pos+8 > len(data) {
				return nil, fmt.Errorf("wasmsql: truncated float64 param")
			}
			params[i] = math.Float64frombits(binary.LittleEndian.Uint64(data[pos:]))
			pos += 8
		case tlvText:
			if pos+4 > len(data) {
				return nil, fmt.Errorf("wasmsql: truncated text length")
			}
			n := binary.LittleEndian.Uint32(data[pos:])
			pos += 4
			if pos+int(n) > len(data) {
				return nil, fmt.Errorf("wasmsql: truncated text data")
			}
			params[i] = string(data[pos : pos+int(n)])
			pos += int(n)
		case tlvBlob:
			if pos+4 > len(data) {
				return nil, fmt.Errorf("wasmsql: truncated blob length")
			}
			n := binary.LittleEndian.Uint32(data[pos:])
			pos += 4
			if pos+int(n) > len(data) {
				return nil, fmt.Errorf("wasmsql: truncated blob data")
			}
			b := make([]byte, n)
			copy(b, data[pos:pos+int(n)])
			params[i] = b
			pos += int(n)
		case tlvBool:
			if pos >= len(data) {
				return nil, fmt.Errorf("wasmsql: truncated bool param")
			}
			params[i] = data[pos] != 0
			pos++
		default:
			return nil, fmt.Errorf("wasmsql: unknown param type 0x%02x", typ)
		}
	}
	return params, nil
}

// WasmSQLNamespace returns a NamespaceProvider that registers a "wasmsql"
// import namespace providing host-side database operations. The driverName
// specifies which database/sql driver to use for connections opened by the
// guest (e.g., "sqlite").
func WasmSQLNamespace(driverName string) NamespaceProvider {
	return func(store *wasmer.Store, inst *GoInstance) (string, map[string]wasmer.IntoExtern) {
		h := newWasmSQLHost(driverName)

		fns := map[string]wasmer.IntoExtern{
			// open(pathPtr i32, pathLen i32) -> i32 (handle or 0)
			"open": wasmer.NewFunction(store,
				wasmer.NewFunctionType(
					wasmer.NewValueTypes(wasmer.I32, wasmer.I32),
					wasmer.NewValueTypes(wasmer.I32),
				),
				func(args []wasmer.Value) ([]wasmer.Value, error) {
					pathPtr := args[0].I32()
					pathLen := args[1].I32()
					dsn := inst.ReadString(pathPtr, pathLen)

					h.mu.Lock()
					defer h.mu.Unlock()

					db, err := sql.Open(h.driverName, dsn)
					if err != nil {
						return []wasmer.Value{wasmer.NewI32(0)}, nil
					}
					// Enable WAL for SQLite; harmless PRAGMA for other drivers
					db.Exec("PRAGMA journal_mode=WAL")

					id := h.allocID()
					h.dbs[id] = db
					return []wasmer.Value{wasmer.NewI32(id)}, nil
				},
			),

			// close(dbHandle i32) -> i32 (0=ok)
			"close": wasmer.NewFunction(store,
				wasmer.NewFunctionType(
					wasmer.NewValueTypes(wasmer.I32),
					wasmer.NewValueTypes(wasmer.I32),
				),
				func(args []wasmer.Value) ([]wasmer.Value, error) {
					dbID := args[0].I32()

					h.mu.Lock()
					defer h.mu.Unlock()

					db, ok := h.dbs[dbID]
					if !ok {
						return []wasmer.Value{wasmer.NewI32(1)}, nil
					}

					// Clean up any open results for this db
					for rid, rs := range h.results {
						if rs.dbID == dbID {
							rs.rows.Close()
							delete(h.results, rid)
						}
					}

					err := db.Close()
					delete(h.dbs, dbID)
					if err != nil {
						return []wasmer.Value{wasmer.NewI32(1)}, nil
					}
					return []wasmer.Value{wasmer.NewI32(0)}, nil
				},
			),

			// exec(dbHandle, sqlPtr, sqlLen, paramsPtr, paramsLen, resultPtr, resultLen) -> i32
			"exec": wasmer.NewFunction(store,
				wasmer.NewFunctionType(
					wasmer.NewValueTypes(wasmer.I32, wasmer.I32, wasmer.I32, wasmer.I32, wasmer.I32, wasmer.I32, wasmer.I32),
					wasmer.NewValueTypes(wasmer.I32),
				),
				func(args []wasmer.Value) ([]wasmer.Value, error) {
					dbID := args[0].I32()
					sqlPtr := args[1].I32()
					sqlLen := args[2].I32()
					paramsPtr := args[3].I32()
					paramsLen := args[4].I32()
					resultPtr := args[5].I32()
					resultLen := args[6].I32()

					query := inst.ReadString(sqlPtr, sqlLen)

					h.mu.Lock()
					db, ok := h.dbs[dbID]
					h.mu.Unlock()

					if !ok {
						return []wasmer.Value{wasmer.NewI32(writeError(inst, resultPtr, resultLen, fmt.Errorf("invalid db handle")))}, nil
					}

					// Decode params
					var params []interface{}
					if paramsLen > 0 {
						paramData := make([]byte, paramsLen)
						copy(paramData, inst.mem.Data()[paramsPtr:paramsPtr+paramsLen])
						var err error
						params, err = decodeParams(paramData)
						if err != nil {
							return []wasmer.Value{wasmer.NewI32(writeError(inst, resultPtr, resultLen, err))}, nil
						}
					}

					result, err := db.Exec(query, params...)
					if err != nil {
						return []wasmer.Value{wasmer.NewI32(writeError(inst, resultPtr, resultLen, err))}, nil
					}

					lastID, _ := result.LastInsertId()
					affected, _ := result.RowsAffected()
					encoded := encodeExecResult(lastID, affected)

					if int32(len(encoded)) > resultLen {
						binary.LittleEndian.PutUint32(inst.mem.Data()[resultPtr:], uint32(len(encoded)))
						return []wasmer.Value{wasmer.NewI32(-1)}, nil
					}

					inst.WriteBytes(resultPtr, encoded)
					return []wasmer.Value{wasmer.NewI32(int32(len(encoded)))}, nil
				},
			),

			// query(dbHandle, sqlPtr, sqlLen, paramsPtr, paramsLen, resultPtr, resultLen) -> i32
			"query": wasmer.NewFunction(store,
				wasmer.NewFunctionType(
					wasmer.NewValueTypes(wasmer.I32, wasmer.I32, wasmer.I32, wasmer.I32, wasmer.I32, wasmer.I32, wasmer.I32),
					wasmer.NewValueTypes(wasmer.I32),
				),
				func(args []wasmer.Value) ([]wasmer.Value, error) {
					dbID := args[0].I32()
					sqlPtr := args[1].I32()
					sqlLen := args[2].I32()
					paramsPtr := args[3].I32()
					paramsLen := args[4].I32()
					resultPtr := args[5].I32()
					resultLen := args[6].I32()

					query := inst.ReadString(sqlPtr, sqlLen)

					h.mu.Lock()
					db, ok := h.dbs[dbID]
					h.mu.Unlock()

					if !ok {
						return []wasmer.Value{wasmer.NewI32(writeError(inst, resultPtr, resultLen, fmt.Errorf("invalid db handle")))}, nil
					}

					// Decode params
					var params []interface{}
					if paramsLen > 0 {
						paramData := make([]byte, paramsLen)
						copy(paramData, inst.mem.Data()[paramsPtr:paramsPtr+paramsLen])
						var err error
						params, err = decodeParams(paramData)
						if err != nil {
							return []wasmer.Value{wasmer.NewI32(writeError(inst, resultPtr, resultLen, err))}, nil
						}
					}

					rows, err := db.Query(query, params...)
					if err != nil {
						return []wasmer.Value{wasmer.NewI32(writeError(inst, resultPtr, resultLen, err))}, nil
					}

					cols, err := rows.Columns()
					if err != nil {
						rows.Close()
						return []wasmer.Value{wasmer.NewI32(writeError(inst, resultPtr, resultLen, err))}, nil
					}

					h.mu.Lock()
					handle := h.allocID()
					h.results[handle] = &resultState{
						rows:    rows,
						cols:    cols,
						numCols: len(cols),
						dbID:    dbID,
					}
					h.mu.Unlock()

					header := encodeQueryHeader(handle, cols)
					if int32(len(header)) > resultLen {
						binary.LittleEndian.PutUint32(inst.mem.Data()[resultPtr:], uint32(len(header)))
						return []wasmer.Value{wasmer.NewI32(-1)}, nil
					}

					inst.WriteBytes(resultPtr, header)
					return []wasmer.Value{wasmer.NewI32(int32(len(header)))}, nil
				},
			),

			// next(resultHandle, rowPtr, rowLen) -> i32
			"next": wasmer.NewFunction(store,
				wasmer.NewFunctionType(
					wasmer.NewValueTypes(wasmer.I32, wasmer.I32, wasmer.I32),
					wasmer.NewValueTypes(wasmer.I32),
				),
				func(args []wasmer.Value) ([]wasmer.Value, error) {
					handle := args[0].I32()
					rowPtr := args[1].I32()
					rowLen := args[2].I32()

					h.mu.Lock()
					rs, ok := h.results[handle]
					h.mu.Unlock()

					if !ok {
						return []wasmer.Value{wasmer.NewI32(writeError(inst, rowPtr, rowLen, fmt.Errorf("invalid result handle")))}, nil
					}

					// If we don't have a cached row, advance the cursor
					if rs.current == nil && !rs.done {
						if !rs.rows.Next() {
							rs.done = true
							if err := rs.rows.Err(); err != nil {
								return []wasmer.Value{wasmer.NewI32(writeError(inst, rowPtr, rowLen, err))}, nil
							}
							return []wasmer.Value{wasmer.NewI32(0)}, nil // EOF
						}
						// Scan the row
						vals := make([]interface{}, rs.numCols)
						ptrs := make([]interface{}, rs.numCols)
						for i := range vals {
							ptrs[i] = &vals[i]
						}
						if err := rs.rows.Scan(ptrs...); err != nil {
							return []wasmer.Value{wasmer.NewI32(writeError(inst, rowPtr, rowLen, err))}, nil
						}
						rs.current = vals
					}

					if rs.done {
						return []wasmer.Value{wasmer.NewI32(0)}, nil // EOF
					}

					encoded := encodeRow(rs.current)
					if int32(len(encoded)) > rowLen {
						// Buffer too small — write required size, don't consume the row
						binary.LittleEndian.PutUint32(inst.mem.Data()[rowPtr:], uint32(len(encoded)))
						return []wasmer.Value{wasmer.NewI32(-1)}, nil
					}

					inst.WriteBytes(rowPtr, encoded)
					rs.current = nil // consumed — next call will advance
					return []wasmer.Value{wasmer.NewI32(int32(len(encoded)))}, nil
				},
			),

			// close_rows(resultHandle) -> i32 (0=ok)
			"close_rows": wasmer.NewFunction(store,
				wasmer.NewFunctionType(
					wasmer.NewValueTypes(wasmer.I32),
					wasmer.NewValueTypes(wasmer.I32),
				),
				func(args []wasmer.Value) ([]wasmer.Value, error) {
					handle := args[0].I32()

					h.mu.Lock()
					defer h.mu.Unlock()

					rs, ok := h.results[handle]
					if !ok {
						return []wasmer.Value{wasmer.NewI32(1)}, nil
					}
					rs.rows.Close()
					delete(h.results, handle)
					return []wasmer.Value{wasmer.NewI32(0)}, nil
				},
			),
		}

		return "wasmsql", fns
	}
}
