package gigwasm

import (
	"database/sql"
	"fmt"
	"sync"

	_ "modernc.org/sqlite"

	"github.com/wasmerio/wasmer-go/wasmer"
)

// SQLite result codes
const (
	sqliteOK   = 0
	sqliteRow  = 100
	sqliteDone = 101

	// Column type codes
	sqliteInteger = 1
	sqliteFloat   = 2
	sqliteText    = 3
	sqliteBlob    = 4
	sqliteNull    = 5
)

type sqliteHost struct {
	mu      sync.Mutex
	dbs     map[int32]*sql.DB
	stmts   map[int32]*stmtState
	nextID  int32
	lastErr map[int32]string // per-db error messages
}

type stmtState struct {
	db      *sql.DB
	dbID    int32
	query   string
	params  map[int]interface{}
	rows    *sql.Rows
	cols    []string
	current []interface{} // cached row values after step()
	done    bool
	result  sql.Result // for exec statements
}

func newSQLiteHost() *sqliteHost {
	return &sqliteHost{
		dbs:     make(map[int32]*sql.DB),
		stmts:   make(map[int32]*stmtState),
		nextID:  1,
		lastErr: make(map[int32]string),
	}
}

func (h *sqliteHost) allocID() int32 {
	id := h.nextID
	h.nextID++
	return id
}

func (h *sqliteHost) setErr(dbID int32, err error) {
	if err != nil {
		h.lastErr[dbID] = err.Error()
	} else {
		delete(h.lastErr, dbID)
	}
}

// isSelectQuery returns true if the query appears to be a SELECT-like statement.
func isSelectQuery(q string) bool {
	// Skip whitespace
	for i := 0; i < len(q); i++ {
		c := q[i]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			continue
		}
		// Check for SELECT, PRAGMA, EXPLAIN, WITH
		if i+6 <= len(q) {
			prefix := q[i : i+6]
			if prefix == "SELECT" || prefix == "select" ||
				prefix == "PRAGMA" || prefix == "pragma" {
				return true
			}
		}
		if i+7 <= len(q) {
			prefix := q[i : i+7]
			if prefix == "EXPLAIN" || prefix == "explain" {
				return true
			}
		}
		if i+4 <= len(q) {
			prefix := q[i : i+4]
			if prefix == "WITH" || prefix == "with" {
				return true
			}
		}
		return false
	}
	return false
}

// SQLiteNamespace returns a NamespaceProvider that registers a "sqlite3" import
// namespace providing host-side SQLite operations via database/sql.
func SQLiteNamespace() NamespaceProvider {
	return func(store *wasmer.Store, inst *GoInstance) (string, map[string]wasmer.IntoExtern) {
		h := newSQLiteHost()

		fns := map[string]wasmer.IntoExtern{
			// open(pathPtr i32, pathLen i32) -> i32 (handle or 0 on error)
			"open": wasmer.NewFunction(store,
				wasmer.NewFunctionType(
					wasmer.NewValueTypes(wasmer.I32, wasmer.I32),
					wasmer.NewValueTypes(wasmer.I32),
				),
				func(args []wasmer.Value) ([]wasmer.Value, error) {
					pathPtr := args[0].I32()
					pathLen := args[1].I32()
					path := inst.ReadString(pathPtr, pathLen)

					h.mu.Lock()
					defer h.mu.Unlock()

					db, err := sql.Open("sqlite", path)
					if err != nil {
						h.setErr(0, err)
						return []wasmer.Value{wasmer.NewI32(0)}, nil
					}
					// Enable WAL mode for better concurrency
					db.Exec("PRAGMA journal_mode=WAL")

					id := h.allocID()
					h.dbs[id] = db
					return []wasmer.Value{wasmer.NewI32(id)}, nil
				},
			),

			// close(db i32) -> i32 (0=ok)
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
					err := db.Close()
					delete(h.dbs, dbID)
					delete(h.lastErr, dbID)
					if err != nil {
						return []wasmer.Value{wasmer.NewI32(1)}, nil
					}
					return []wasmer.Value{wasmer.NewI32(sqliteOK)}, nil
				},
			),

			// errmsg(db i32, bufPtr i32, bufLen i32) -> i32 (bytes written)
			"errmsg": wasmer.NewFunction(store,
				wasmer.NewFunctionType(
					wasmer.NewValueTypes(wasmer.I32, wasmer.I32, wasmer.I32),
					wasmer.NewValueTypes(wasmer.I32),
				),
				func(args []wasmer.Value) ([]wasmer.Value, error) {
					dbID := args[0].I32()
					bufPtr := args[1].I32()
					bufLen := args[2].I32()

					h.mu.Lock()
					msg := h.lastErr[dbID]
					h.mu.Unlock()

					if msg == "" {
						msg = "not an error"
					}
					b := []byte(msg)
					if int32(len(b)) > bufLen {
						b = b[:bufLen]
					}
					n := inst.WriteBytes(bufPtr, b)
					return []wasmer.Value{wasmer.NewI32(int32(n))}, nil
				},
			),

			// prepare(db i32, sqlPtr i32, sqlLen i32) -> i32 (stmt handle or 0)
			"prepare": wasmer.NewFunction(store,
				wasmer.NewFunctionType(
					wasmer.NewValueTypes(wasmer.I32, wasmer.I32, wasmer.I32),
					wasmer.NewValueTypes(wasmer.I32),
				),
				func(args []wasmer.Value) ([]wasmer.Value, error) {
					dbID := args[0].I32()
					sqlPtr := args[1].I32()
					sqlLen := args[2].I32()
					query := inst.ReadString(sqlPtr, sqlLen)

					h.mu.Lock()
					defer h.mu.Unlock()

					db, ok := h.dbs[dbID]
					if !ok {
						h.setErr(dbID, fmt.Errorf("invalid db handle"))
						return []wasmer.Value{wasmer.NewI32(0)}, nil
					}

					id := h.allocID()
					h.stmts[id] = &stmtState{
						db:     db,
						dbID:   dbID,
						query:  query,
						params: make(map[int]interface{}),
					}
					return []wasmer.Value{wasmer.NewI32(id)}, nil
				},
			),

			// finalize(stmt i32) -> i32 (0=ok)
			"finalize": wasmer.NewFunction(store,
				wasmer.NewFunctionType(
					wasmer.NewValueTypes(wasmer.I32),
					wasmer.NewValueTypes(wasmer.I32),
				),
				func(args []wasmer.Value) ([]wasmer.Value, error) {
					stmtID := args[0].I32()

					h.mu.Lock()
					defer h.mu.Unlock()

					st, ok := h.stmts[stmtID]
					if !ok {
						return []wasmer.Value{wasmer.NewI32(1)}, nil
					}
					if st.rows != nil {
						st.rows.Close()
					}
					delete(h.stmts, stmtID)
					return []wasmer.Value{wasmer.NewI32(sqliteOK)}, nil
				},
			),

			// reset(stmt i32) -> i32 (0=ok)
			"reset": wasmer.NewFunction(store,
				wasmer.NewFunctionType(
					wasmer.NewValueTypes(wasmer.I32),
					wasmer.NewValueTypes(wasmer.I32),
				),
				func(args []wasmer.Value) ([]wasmer.Value, error) {
					stmtID := args[0].I32()

					h.mu.Lock()
					defer h.mu.Unlock()

					st, ok := h.stmts[stmtID]
					if !ok {
						return []wasmer.Value{wasmer.NewI32(1)}, nil
					}
					if st.rows != nil {
						st.rows.Close()
						st.rows = nil
					}
					st.cols = nil
					st.current = nil
					st.done = false
					st.result = nil
					st.params = make(map[int]interface{})
					return []wasmer.Value{wasmer.NewI32(sqliteOK)}, nil
				},
			),

			// bind_text(stmt i32, idx i32, ptr i32, len i32) -> i32 (0=ok)
			"bind_text": wasmer.NewFunction(store,
				wasmer.NewFunctionType(
					wasmer.NewValueTypes(wasmer.I32, wasmer.I32, wasmer.I32, wasmer.I32),
					wasmer.NewValueTypes(wasmer.I32),
				),
				func(args []wasmer.Value) ([]wasmer.Value, error) {
					stmtID := args[0].I32()
					idx := int(args[1].I32())
					ptr := args[2].I32()
					length := args[3].I32()
					val := inst.ReadString(ptr, length)

					h.mu.Lock()
					defer h.mu.Unlock()

					st, ok := h.stmts[stmtID]
					if !ok {
						return []wasmer.Value{wasmer.NewI32(1)}, nil
					}
					st.params[idx] = val
					return []wasmer.Value{wasmer.NewI32(sqliteOK)}, nil
				},
			),

			// bind_int64(stmt i32, idx i32, val i64) -> i32 (0=ok)
			"bind_int64": wasmer.NewFunction(store,
				wasmer.NewFunctionType(
					wasmer.NewValueTypes(wasmer.I32, wasmer.I32, wasmer.I64),
					wasmer.NewValueTypes(wasmer.I32),
				),
				func(args []wasmer.Value) ([]wasmer.Value, error) {
					stmtID := args[0].I32()
					idx := int(args[1].I32())
					val := args[2].I64()

					h.mu.Lock()
					defer h.mu.Unlock()

					st, ok := h.stmts[stmtID]
					if !ok {
						return []wasmer.Value{wasmer.NewI32(1)}, nil
					}
					st.params[idx] = val
					return []wasmer.Value{wasmer.NewI32(sqliteOK)}, nil
				},
			),

			// bind_double(stmt i32, idx i32, val f64) -> i32 (0=ok)
			"bind_double": wasmer.NewFunction(store,
				wasmer.NewFunctionType(
					wasmer.NewValueTypes(wasmer.I32, wasmer.I32, wasmer.F64),
					wasmer.NewValueTypes(wasmer.I32),
				),
				func(args []wasmer.Value) ([]wasmer.Value, error) {
					stmtID := args[0].I32()
					idx := int(args[1].I32())
					val := args[2].F64()

					h.mu.Lock()
					defer h.mu.Unlock()

					st, ok := h.stmts[stmtID]
					if !ok {
						return []wasmer.Value{wasmer.NewI32(1)}, nil
					}
					st.params[idx] = val
					return []wasmer.Value{wasmer.NewI32(sqliteOK)}, nil
				},
			),

			// bind_null(stmt i32, idx i32) -> i32 (0=ok)
			"bind_null": wasmer.NewFunction(store,
				wasmer.NewFunctionType(
					wasmer.NewValueTypes(wasmer.I32, wasmer.I32),
					wasmer.NewValueTypes(wasmer.I32),
				),
				func(args []wasmer.Value) ([]wasmer.Value, error) {
					stmtID := args[0].I32()
					idx := int(args[1].I32())

					h.mu.Lock()
					defer h.mu.Unlock()

					st, ok := h.stmts[stmtID]
					if !ok {
						return []wasmer.Value{wasmer.NewI32(1)}, nil
					}
					st.params[idx] = nil
					return []wasmer.Value{wasmer.NewI32(sqliteOK)}, nil
				},
			),

			// step(stmt i32) -> i32 (100=ROW, 101=DONE, other=error)
			"step": wasmer.NewFunction(store,
				wasmer.NewFunctionType(
					wasmer.NewValueTypes(wasmer.I32),
					wasmer.NewValueTypes(wasmer.I32),
				),
				func(args []wasmer.Value) ([]wasmer.Value, error) {
					stmtID := args[0].I32()

					h.mu.Lock()
					defer h.mu.Unlock()

					st, ok := h.stmts[stmtID]
					if !ok {
						return []wasmer.Value{wasmer.NewI32(1)}, nil
					}

					// First call to step: execute the query
					if st.rows == nil && !st.done {
						// Collect params in order
						paramSlice := make([]interface{}, 0, len(st.params))
						for i := 1; i <= len(st.params); i++ {
							paramSlice = append(paramSlice, st.params[i])
						}

						if isSelectQuery(st.query) {
							rows, err := st.db.Query(st.query, paramSlice...)
							if err != nil {
								h.setErr(st.dbID, err)
								st.done = true
								return []wasmer.Value{wasmer.NewI32(1)}, nil
							}
							st.rows = rows
							cols, err := rows.Columns()
							if err != nil {
								h.setErr(st.dbID, err)
								st.done = true
								return []wasmer.Value{wasmer.NewI32(1)}, nil
							}
							st.cols = cols
						} else {
							result, err := st.db.Exec(st.query, paramSlice...)
							if err != nil {
								h.setErr(st.dbID, err)
								st.done = true
								return []wasmer.Value{wasmer.NewI32(1)}, nil
							}
							st.result = result
							st.done = true
							return []wasmer.Value{wasmer.NewI32(sqliteDone)}, nil
						}
					}

					if st.done || st.rows == nil {
						return []wasmer.Value{wasmer.NewI32(sqliteDone)}, nil
					}

					// Advance to next row
					if !st.rows.Next() {
						if err := st.rows.Err(); err != nil {
							h.setErr(st.dbID, err)
							return []wasmer.Value{wasmer.NewI32(1)}, nil
						}
						st.done = true
						return []wasmer.Value{wasmer.NewI32(sqliteDone)}, nil
					}

					// Read row values
					vals := make([]interface{}, len(st.cols))
					ptrs := make([]interface{}, len(st.cols))
					for i := range vals {
						ptrs[i] = &vals[i]
					}
					if err := st.rows.Scan(ptrs...); err != nil {
						h.setErr(st.dbID, err)
						return []wasmer.Value{wasmer.NewI32(1)}, nil
					}
					st.current = vals
					return []wasmer.Value{wasmer.NewI32(sqliteRow)}, nil
				},
			),

			// column_count(stmt i32) -> i32
			"column_count": wasmer.NewFunction(store,
				wasmer.NewFunctionType(
					wasmer.NewValueTypes(wasmer.I32),
					wasmer.NewValueTypes(wasmer.I32),
				),
				func(args []wasmer.Value) ([]wasmer.Value, error) {
					stmtID := args[0].I32()

					h.mu.Lock()
					defer h.mu.Unlock()

					st, ok := h.stmts[stmtID]
					if !ok {
						return []wasmer.Value{wasmer.NewI32(0)}, nil
					}
					return []wasmer.Value{wasmer.NewI32(int32(len(st.cols)))}, nil
				},
			),

			// column_type(stmt i32, col i32) -> i32 (type code)
			"column_type": wasmer.NewFunction(store,
				wasmer.NewFunctionType(
					wasmer.NewValueTypes(wasmer.I32, wasmer.I32),
					wasmer.NewValueTypes(wasmer.I32),
				),
				func(args []wasmer.Value) ([]wasmer.Value, error) {
					stmtID := args[0].I32()
					col := int(args[1].I32())

					h.mu.Lock()
					defer h.mu.Unlock()

					st, ok := h.stmts[stmtID]
					if !ok || st.current == nil || col >= len(st.current) {
						return []wasmer.Value{wasmer.NewI32(sqliteNull)}, nil
					}

					v := st.current[col]
					if v == nil {
						return []wasmer.Value{wasmer.NewI32(sqliteNull)}, nil
					}

					switch v.(type) {
					case int64:
						return []wasmer.Value{wasmer.NewI32(sqliteInteger)}, nil
					case float64:
						return []wasmer.Value{wasmer.NewI32(sqliteFloat)}, nil
					case string:
						return []wasmer.Value{wasmer.NewI32(sqliteText)}, nil
					case []byte:
						// Check if it looks like text
						return []wasmer.Value{wasmer.NewI32(sqliteText)}, nil
					default:
						return []wasmer.Value{wasmer.NewI32(sqliteText)}, nil
					}
				},
			),

			// column_text(stmt i32, col i32, bufPtr i32, bufLen i32) -> i32 (bytes written)
			"column_text": wasmer.NewFunction(store,
				wasmer.NewFunctionType(
					wasmer.NewValueTypes(wasmer.I32, wasmer.I32, wasmer.I32, wasmer.I32),
					wasmer.NewValueTypes(wasmer.I32),
				),
				func(args []wasmer.Value) ([]wasmer.Value, error) {
					stmtID := args[0].I32()
					col := int(args[1].I32())
					bufPtr := args[2].I32()
					bufLen := args[3].I32()

					h.mu.Lock()
					defer h.mu.Unlock()

					st, ok := h.stmts[stmtID]
					if !ok || st.current == nil || col >= len(st.current) {
						return []wasmer.Value{wasmer.NewI32(0)}, nil
					}

					var s string
					switch v := st.current[col].(type) {
					case string:
						s = v
					case []byte:
						s = string(v)
					case int64:
						s = fmt.Sprintf("%d", v)
					case float64:
						s = fmt.Sprintf("%g", v)
					case nil:
						s = ""
					default:
						s = fmt.Sprintf("%v", v)
					}

					b := []byte(s)
					if int32(len(b)) > bufLen {
						b = b[:bufLen]
					}
					n := inst.WriteBytes(bufPtr, b)
					return []wasmer.Value{wasmer.NewI32(int32(n))}, nil
				},
			),

			// column_int64(stmt i32, col i32) -> i64
			"column_int64": wasmer.NewFunction(store,
				wasmer.NewFunctionType(
					wasmer.NewValueTypes(wasmer.I32, wasmer.I32),
					wasmer.NewValueTypes(wasmer.I64),
				),
				func(args []wasmer.Value) ([]wasmer.Value, error) {
					stmtID := args[0].I32()
					col := int(args[1].I32())

					h.mu.Lock()
					defer h.mu.Unlock()

					st, ok := h.stmts[stmtID]
					if !ok || st.current == nil || col >= len(st.current) {
						return []wasmer.Value{wasmer.NewI64(0)}, nil
					}

					switch v := st.current[col].(type) {
					case int64:
						return []wasmer.Value{wasmer.NewI64(v)}, nil
					case float64:
						return []wasmer.Value{wasmer.NewI64(int64(v))}, nil
					default:
						return []wasmer.Value{wasmer.NewI64(0)}, nil
					}
				},
			),

			// column_double(stmt i32, col i32) -> f64
			"column_double": wasmer.NewFunction(store,
				wasmer.NewFunctionType(
					wasmer.NewValueTypes(wasmer.I32, wasmer.I32),
					wasmer.NewValueTypes(wasmer.F64),
				),
				func(args []wasmer.Value) ([]wasmer.Value, error) {
					stmtID := args[0].I32()
					col := int(args[1].I32())

					h.mu.Lock()
					defer h.mu.Unlock()

					st, ok := h.stmts[stmtID]
					if !ok || st.current == nil || col >= len(st.current) {
						return []wasmer.Value{wasmer.NewF64(0)}, nil
					}

					switch v := st.current[col].(type) {
					case float64:
						return []wasmer.Value{wasmer.NewF64(v)}, nil
					case int64:
						return []wasmer.Value{wasmer.NewF64(float64(v))}, nil
					default:
						return []wasmer.Value{wasmer.NewF64(0)}, nil
					}
				},
			),

			// column_name(stmt i32, col i32, bufPtr i32, bufLen i32) -> i32 (bytes written)
			"column_name": wasmer.NewFunction(store,
				wasmer.NewFunctionType(
					wasmer.NewValueTypes(wasmer.I32, wasmer.I32, wasmer.I32, wasmer.I32),
					wasmer.NewValueTypes(wasmer.I32),
				),
				func(args []wasmer.Value) ([]wasmer.Value, error) {
					stmtID := args[0].I32()
					col := int(args[1].I32())
					bufPtr := args[2].I32()
					bufLen := args[3].I32()

					h.mu.Lock()
					defer h.mu.Unlock()

					st, ok := h.stmts[stmtID]
					if !ok || col >= len(st.cols) {
						return []wasmer.Value{wasmer.NewI32(0)}, nil
					}

					b := []byte(st.cols[col])
					if int32(len(b)) > bufLen {
						b = b[:bufLen]
					}
					n := inst.WriteBytes(bufPtr, b)
					return []wasmer.Value{wasmer.NewI32(int32(n))}, nil
				},
			),

			// last_insert_rowid(db i32) -> i64
			"last_insert_rowid": wasmer.NewFunction(store,
				wasmer.NewFunctionType(
					wasmer.NewValueTypes(wasmer.I32),
					wasmer.NewValueTypes(wasmer.I64),
				),
				func(args []wasmer.Value) ([]wasmer.Value, error) {
					// We get the last insert rowid from the most recent exec result
					// stored on a stmt. Since database/sql doesn't expose per-db rowid,
					// we iterate stmts to find the last one with a result for this db.
					dbID := args[0].I32()

					h.mu.Lock()
					defer h.mu.Unlock()

					// Find the most recently used stmt for this db that has a result
					var lastResult sql.Result
					for _, st := range h.stmts {
						if st.dbID == dbID && st.result != nil {
							lastResult = st.result
						}
					}
					if lastResult == nil {
						return []wasmer.Value{wasmer.NewI64(0)}, nil
					}
					id, err := lastResult.LastInsertId()
					if err != nil {
						return []wasmer.Value{wasmer.NewI64(0)}, nil
					}
					return []wasmer.Value{wasmer.NewI64(id)}, nil
				},
			),

			// changes(db i32) -> i32
			"changes": wasmer.NewFunction(store,
				wasmer.NewFunctionType(
					wasmer.NewValueTypes(wasmer.I32),
					wasmer.NewValueTypes(wasmer.I32),
				),
				func(args []wasmer.Value) ([]wasmer.Value, error) {
					dbID := args[0].I32()

					h.mu.Lock()
					defer h.mu.Unlock()

					var lastResult sql.Result
					for _, st := range h.stmts {
						if st.dbID == dbID && st.result != nil {
							lastResult = st.result
						}
					}
					if lastResult == nil {
						return []wasmer.Value{wasmer.NewI32(0)}, nil
					}
					n, err := lastResult.RowsAffected()
					if err != nil {
						return []wasmer.Value{wasmer.NewI32(0)}, nil
					}
					return []wasmer.Value{wasmer.NewI32(int32(n))}, nil
				},
			),
		}

		return "sqlite3", fns
	}
}
