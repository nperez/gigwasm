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
- **`sqlitehost.go`** — `SQLiteNamespace()` returns a `NamespaceProvider` that registers a `sqlite3` import namespace backed by `modernc.org/sqlite`. Provides prepare/step/bind/column functions matching the SQLite C API pattern.

### Guest-side SQL driver (`wasmsql/driver.go`)

A `database/sql` driver meant to be compiled *into* the WASM module (build tag `js && wasm`). Uses `//go:wasmimport sqlite3 ...` to call host functions from `SQLiteNamespace()`. Registers as driver name `"sqlite"`.

### Extension pattern

Custom host functionality is added via `NamespaceProvider` functions passed through `WithImportNamespace()`. A provider receives the wasmer Store and GoInstance, and returns a namespace name + map of exported functions.
