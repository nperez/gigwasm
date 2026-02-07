# gigwasm

(AI SLOP: I started this by hand quite some time ago and it became too tedious to get working, but the robots are now good enough that I can carefully direct what I want and how to verify it that this is now a thing and I am now unblocked in other projects)

A pure-Go host for running Go-compiled WebAssembly modules outside the browser. gigwasm replaces the JavaScript `wasm_exec.js` runtime with native Go, using [wasmer-go](https://github.com/wasmerio/wasmer-go) to execute WASM.

Supports modules compiled with both standard Go (`GOOS=js GOARCH=wasm`) and TinyGo (`-target wasm`), with automatic ABI detection.

## Usage

```go
wasmBytes, _ := gigwasm.CompileGo("./my-wasm-app")
inst, _ := gigwasm.NewInstance(wasmBytes)

// Call an exported function
fn := inst.Get("MyFunc").(func([]interface{}) interface{})
result := fn([]interface{}{float64(42)})
```

### Options

```go
// Enable net/http support via Fetch API
inst, _ := gigwasm.NewInstance(wasmBytes, gigwasm.WithFetch())

// Use a custom HTTP client
inst, _ := gigwasm.NewInstance(wasmBytes, gigwasm.WithFetchClient(myClient))

// Pass command-line arguments
inst, _ := gigwasm.NewInstance(wasmBytes, gigwasm.WithArgs([]string{"app", "--flag"}))

// Add host-side SQLite
inst, _ := gigwasm.NewInstance(wasmBytes,
    gigwasm.WithImportNamespace(gigwasm.SQLiteNamespace()),
)
```

## Runtime Support

### Standard Go ABI (`GOOS=js GOARCH=wasm`)

gigwasm implements all 25 functions in the `gojs` import namespace that Go's `wasm_exec.js` provides.

#### Runtime functions

| Function | Status | Notes |
|---|---|---|
| `debug` | Supported | |
| `runtime.wasmExit` | Supported | |
| `runtime.wasmWrite` | Supported | stdout and stderr |
| `runtime.resetMemoryDataView` | Supported | No-op (wasmer doesn't change buffer identity) |
| `runtime.nanotime1` | Supported | |
| `runtime.walltime` | Supported | |
| `runtime.scheduleTimeoutEvent` | Stub | No-op; no async event loop |
| `runtime.clearTimeoutEvent` | Stub | No-op |
| `runtime.getRandomData` | Supported | Uses `crypto/rand` |

#### syscall/js interop

| Function | Status | Notes |
|---|---|---|
| `syscall/js.finalizeRef` | Supported | Full ref-counting with ID pool recycling |
| `syscall/js.stringVal` | Supported | |
| `syscall/js.valueGet` | Supported | Re-reads sp via `getsp()` after call |
| `syscall/js.valueSet` | Supported | |
| `syscall/js.valueDelete` | Supported | |
| `syscall/js.valueIndex` | Supported | |
| `syscall/js.valueSetIndex` | Supported | |
| `syscall/js.valueCall` | Supported | Re-reads sp via `getsp()` after call |
| `syscall/js.valueInvoke` | Supported | Re-reads sp via `getsp()` after call |
| `syscall/js.valueNew` | Supported | Re-reads sp via `getsp()` after call |
| `syscall/js.valueLength` | Supported | Works on slices, byte arrays, and strings |
| `syscall/js.valuePrepareString` | Supported | |
| `syscall/js.valueLoadString` | Supported | |
| `syscall/js.valueInstanceOf` | Stub | Always returns false |
| `syscall/js.copyBytesToGo` | Supported | |
| `syscall/js.copyBytesToJS` | Supported | |

### TinyGo ABI (`-target wasm`)

gigwasm implements all 18 functions in the `gojs` module and all 6 WASI functions that TinyGo's `wasm_exec.js` provides. The `gojs` namespace is also registered under `env` for Go 1.20 compatibility.

#### gojs module

| Function | Status | Notes |
|---|---|---|
| `runtime.ticks` | Supported | Millisecond-precision float64 |
| `runtime.sleepTicks` | Supported | Calls `go_scheduler` export after sleep |
| `syscall/js.finalizeRef` | Supported | No-op (TinyGo doesn't use ref-counted GC) |
| `syscall/js.stringVal` | Supported | |
| `syscall/js.valueGet` | Supported | |
| `syscall/js.valueSet` | Supported | |
| `syscall/js.valueDelete` | Supported | |
| `syscall/js.valueIndex` | Supported | |
| `syscall/js.valueSetIndex` | Supported | |
| `syscall/js.valueCall` | Supported | |
| `syscall/js.valueInvoke` | Supported | |
| `syscall/js.valueNew` | Supported | |
| `syscall/js.valueLength` | Supported | |
| `syscall/js.valuePrepareString` | Supported | |
| `syscall/js.valueLoadString` | Supported | |
| `syscall/js.valueInstanceOf` | Stub | Always returns false |
| `syscall/js.copyBytesToGo` | Supported | |
| `syscall/js.copyBytesToJS` | Supported | |

#### wasi_snapshot_preview1 module

| Function | Status | Notes |
|---|---|---|
| `fd_write` | Supported | stdout and stderr; line-buffered output |
| `fd_close` | Stub | Returns success |
| `fd_fdstat_get` | Stub | Returns success |
| `fd_seek` | Stub | Returns success |
| `proc_exit` | Supported | Sets exit flag |
| `random_get` | Supported | Uses `crypto/rand` |

### Global Object (JS Environment)

The global object (value slot 5) simulates the browser/Node.js environment that Go WASM modules expect.

| Global | Status | Notes |
|---|---|---|
| `console.log` | Supported | Writes to stdout |
| `console.error` | Supported | Writes to stderr |
| `Object` | Supported | Constructor returns empty map |
| `Array` | Supported | Constructor returns empty slice |
| `Uint8Array` | Supported | Constructor from size or existing bytes |
| `crypto.getRandomValues` | Supported | Uses `crypto/rand` |
| `fs.write` | Supported | stdout/stderr with callback |
| `fs.read` | Supported | stdin with callback |
| `fs.fstat` | Supported | Returns device type (char device vs pipe) |
| `fs.constants` | Supported | `O_WRONLY`, `O_RDWR`, `O_CREAT`, `O_TRUNC`, `O_APPEND`, `O_EXCL` |
| `fs.*` (other) | Stubs | 15 filesystem ops return nil (chmod, mkdir, open, stat, etc.) |
| `process` | Partial | Empty map; missing `getuid`, `getgid`, `umask`, `cwd`, `chdir`, `env`, `argv` |
| `performance` | Not provided | Go uses `runtime.nanotime1` internally, but user code accessing `performance` via `syscall/js` won't find it |
| `TextEncoder` | Not provided | Only needed if user code accesses it directly via `syscall/js` |
| `TextDecoder` | Not provided | Only needed if user code accesses it directly via `syscall/js` |
| `Date` | Not provided | |

### Optional Host Features

#### Fetch API (`WithFetch` / `WithFetchClient`)

Installs a synchronous implementation of the WHATWG Fetch API, enabling `net/http` in standard-Go WASM modules.

| Component | Status |
|---|---|
| `fetch(url, options)` | Supported (GET, POST, custom methods, headers, body) |
| `Headers` (append, set, get, entries) | Supported |
| `Response` (status, headers, body, arrayBuffer) | Supported |
| `ReadableStream` reader (read, cancel) | Supported (delivers all data in one chunk) |
| `Promise` (then with resolve/reject) | Supported (synchronous resolution) |

Limitations: All requests are synchronous. No streaming. No `AbortController`.

#### SQLite Host (`SQLiteNamespace`)

Provides a `sqlite3` import namespace backed by `modernc.org/sqlite`, paired with a guest-side `database/sql` driver in `wasmsql/`.

| Operation | Status |
|---|---|
| `open` / `close` | Supported |
| `prepare` / `finalize` / `reset` | Supported |
| `bind_text` / `bind_int64` / `bind_double` / `bind_null` | Supported |
| `step` | Supported (auto-detects SELECT vs exec) |
| `column_count` / `column_type` / `column_name` | Supported |
| `column_text` / `column_int64` / `column_double` | Supported |
| `last_insert_rowid` / `changes` | Supported |
| `errmsg` | Supported |
| `bind_blob` | Not yet implemented |
| `column_blob` | Not yet implemented (text fallback works for most cases) |

## What Works Well

- **`fmt.Println`** and all stdout/stderr output
- **`syscall/js`** value manipulation (Get, Set, Call, New, etc.)
- **Exporting Go functions** to the host via the global object
- **`crypto/rand`**
- **`time.Now()`** and monotonic clocks
- **`net/http`** client requests (with `WithFetch()`)
- **`database/sql`** with SQLite (with `SQLiteNamespace()` + `wasmsql` driver)

## Known Limitations

- **No async event loop** -- `scheduleTimeoutEvent` and `clearTimeoutEvent` are no-ops. Code relying on `time.Sleep`, `time.After`, or goroutine scheduling across JS callbacks won't work as expected.
- **`valueInstanceOf` always returns false** -- no type hierarchy tracking.
- **No real filesystem** -- most `fs` operations are stubs. Only stdin/stdout/stderr are functional.
- **No DOM or browser APIs** -- this is a server-side/CLI runtime.
- **Promises resolve synchronously** -- the Fetch API works because Go's HTTP transport calls `.then()` immediately, but code expecting async promise behavior will not work.
- **`process` is empty** -- Go code that reads `process.env`, `process.argv`, etc. through `syscall/js` will get nil.

## Extending

Add custom host functionality by implementing a `NamespaceProvider`:

```go
myNamespace := func(store *wasmer.Store, inst *gigwasm.GoInstance) (string, map[string]wasmer.IntoExtern) {
    return "myhost", map[string]wasmer.IntoExtern{
        "hello": wasmer.NewFunction(store,
            wasmer.NewFunctionType(
                wasmer.NewValueTypes(wasmer.I32, wasmer.I32),
                wasmer.NewValueTypes(),
            ),
            func(args []wasmer.Value) ([]wasmer.Value, error) {
                ptr, length := args[0].I32(), args[1].I32()
                name := inst.ReadString(ptr, length)
                fmt.Printf("Hello, %s!\n", name)
                return []wasmer.Value{}, nil
            },
        ),
    }
}

inst, _ := gigwasm.NewInstance(wasmBytes, gigwasm.WithImportNamespace(myNamespace))
```

Guest side (compiled with `GOOS=js GOARCH=wasm`):

```go
//go:wasmimport myhost hello
func hostHello(ptr unsafe.Pointer, length int32)
```
