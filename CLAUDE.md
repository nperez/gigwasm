# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Test

```bash
go build ./...
go test ./...
go test -run TestCompileAndRunStdGo   # run a single test
go test -v ./...                       # verbose output
```

Tests require `go` in PATH; TinyGo tests additionally require `tinygo`. Tests compile Go source from `testdata/stdgo/`, `testdata/tinygo/`, and `testdata/stdgo_fetch/` into WASM and run it.

## Architecture

gigwasm is a Go library that runs Go-compiled WASM modules outside the browser using [wasmer-go](https://github.com/wasmerio/wasmer-go). It replaces the JavaScript host environment that `GOOS=js GOARCH=wasm` normally expects.

### Core runtime (`gigwasm.go`)

- `NewInstance(wasmBytes, ...InstanceOption)` — compiles WASM, auto-detects ABI (standard Go vs TinyGo), registers import namespaces, and runs the module.
- `GoInstance` holds WASM memory, the NaN-boxed value table (indices 0–6 are reserved: NaN, 0, null, true, false, global object, Go runtime object), and ref-counting for GC.
- The value system uses NaN-boxing: floats stored directly, objects/strings/functions get an ID in `values[]` with type flags in the upper 32 bits.
- `CompileGo()` / `CompileTinyGo()` — shell out to `go build` / `tinygo build` to produce WASM bytes.

### ABI-specific runtimes

- **`stdgo_runtime.go`** — implements the `gojs` import namespace for standard Go (`GOOS=js GOARCH=wasm`). All functions take a single `i32` (stack pointer) parameter. Must call `getsp()` after any call that could trigger Go GC (valueGet, valueCall, valueInvoke, valueNew).
- **`tinygo_runtime.go`** — implements `gojs` + `wasi_snapshot_preview1` for TinyGo. Functions use individual typed parameters and return NaN-boxed `i64` refs. Also registers under `env` for Go 1.20 compatibility.

### Host features (InstanceOption pattern)

- **`fetchhost.go`** — `WithFetch()` / `WithFetchClient()` installs a synchronous Fetch API (fetch, Headers, Promise, ReadableStream) in the global object, enabling `net/http` in standard-Go WASM modules.
- **`wasmsqlhost.go`** — `WasmSQLNamespace(driverName string)` returns a `NamespaceProvider` that registers a `wasmsql` import namespace — a streaming database passthrough (virtio for databases). The `driverName` selects the `database/sql` backend (e.g., `"sqlite"` for `modernc.org/sqlite`). Any SQL database works; the guest doesn't know or care what's behind the pipe.

### wasmsql protocol (`wasmsql` namespace)

6 functions using binary TLV on the wire (little-endian):

- **`open`** / **`close`** — open/close a database via `sql.Open(driverName, dsn)`
- **`exec`** — execute non-row-returning SQL (INSERT/UPDATE/DELETE/DDL). Params as TLV, returns `[lastInsertId: i64][rowsAffected: i64]`.
- **`query`** — execute row-returning SQL. Returns a query header with a result handle and column names.
- **`next`** — read the next row from a result handle. Returns one row as TLV values, 0 for EOF. Supports buffer-too-small retry (return -1, required size at buf[0:4]).
- **`close_rows`** — close a result handle and free host resources.

TLV type bytes: `0x00`=null, `0x01`=int64, `0x02`=float64, `0x03`=text, `0x04`=blob, `0x05`=bool. Error convention: return `-(errLen+2)`, UTF-8 message in result buffer.

### Guest-side SQL driver (`wasmsql/driver.go`)

A `database/sql` driver compiled *into* the WASM module (build tag `js && wasm`). Uses `//go:wasmimport wasmsql ...` to call the 6 host functions. Registers as driver name `"wasmsql"`. Stateless prepared statements (just stores query string), streaming `Rows` with reusable row buffer. Zero binary size impact — no `encoding/json`, only raw byte manipulation + `math`.

### Extension pattern

Custom host functionality is added via `NamespaceProvider` functions passed through `WithImportNamespace()`. A provider receives the wasmer Store and GoInstance, and returns a namespace name + map of exported functions.
