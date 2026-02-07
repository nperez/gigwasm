package gigwasm

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"

	"github.com/wasmerio/wasmer-go/wasmer"
)

// ABI represents the detected ABI type of a WASM module.
type ABI int

const (
	ABIStdGo  ABI = iota // Standard Go compiler (GOOS=js GOARCH=wasm)
	ABITinyGo            // TinyGo compiler (-target wasm)
)

// NamespaceProvider is a function that creates a named import namespace.
// It receives the wasmer store and GoInstance, and returns the namespace name
// and a map of export names to wasmer externs.
type NamespaceProvider func(*wasmer.Store, *GoInstance) (string, map[string]wasmer.IntoExtern)

// InstanceOption configures a GoInstance.
type InstanceOption func(*instanceConfig)

type instanceConfig struct {
	namespaces      []NamespaceProvider
	args            []string
	globalModifiers []func(map[string]interface{})
}

// WithImportNamespace registers an additional import namespace for the WASM module.
func WithImportNamespace(p NamespaceProvider) InstanceOption {
	return func(cfg *instanceConfig) {
		cfg.namespaces = append(cfg.namespaces, p)
	}
}

// WithArgs sets the command-line arguments visible to the WASM module.
func WithArgs(args []string) InstanceOption {
	return func(cfg *instanceConfig) {
		cfg.args = args
	}
}

// withGlobalModifier registers a function that will modify the global object
// after initValues(). Used internally by features like WithFetch().
func withGlobalModifier(fn func(map[string]interface{})) InstanceOption {
	return func(cfg *instanceConfig) {
		cfg.globalModifiers = append(cfg.globalModifiers, fn)
	}
}

// GoInstance is instance of Go Runtime.
type GoInstance struct {
	inst        *wasmer.Instance
	mem         *wasmer.Memory
	getsp       wasmer.NativeFunction // standard Go only
	resume      wasmer.NativeFunction
	values      []interface{}
	goRefCounts []int
	ids         map[string]uint64
	idPool      []uint64
	exited      bool
	exitCode    int
	abi         ABI
}

// Get returns a Go value specified by name from the global object.
func (d *GoInstance) Get(name string) interface{} {
	globals := d.values[5].(map[string]interface{})
	return globals[name]
}

// ExitCode returns the exit code set by the WASM module via runtime.wasmExit.
func (d *GoInstance) ExitCode() int {
	return d.exitCode
}

// --- Memory helpers ---

func (d *GoInstance) getInt32(addr int32) int32 {
	return int32(binary.LittleEndian.Uint32(d.mem.Data()[addr:]))
}

func (d *GoInstance) getInt64(addr int32) int64 {
	low := binary.LittleEndian.Uint32(d.mem.Data()[addr:])
	high := binary.LittleEndian.Uint32(d.mem.Data()[addr+4:])
	return int64(low) + int64(high)*4294967296
}

func (d *GoInstance) getUint8(addr int32) byte {
	return d.mem.Data()[addr]
}

func (d *GoInstance) setInt32(addr int32, v int64) {
	binary.LittleEndian.PutUint32(d.mem.Data()[addr:], uint32(v))
}

func (d *GoInstance) setInt64(addr int32, v int64) {
	binary.LittleEndian.PutUint32(d.mem.Data()[addr:], uint32(v))
	binary.LittleEndian.PutUint32(d.mem.Data()[addr+4:], uint32(v/4294967296))
}

func (d *GoInstance) setUint8(addr int32, v byte) {
	d.mem.Data()[addr] = v
}

func (d *GoInstance) setUint32(addr int32, v uint32) {
	binary.LittleEndian.PutUint32(d.mem.Data()[addr:], v)
}

// ReadString reads a string from WASM memory at the given pointer and length.
func (d *GoInstance) ReadString(ptr, length int32) string {
	return string(d.mem.Data()[ptr : ptr+length])
}

// WriteBytes writes data into WASM memory at the given pointer.
// Returns the number of bytes written.
func (d *GoInstance) WriteBytes(ptr int32, data []byte) int {
	return copy(d.mem.Data()[ptr:], data)
}

// Mem returns the raw WASM memory. Use with care.
func (d *GoInstance) Mem() *wasmer.Memory {
	return d.mem
}

// --- Value system ---

const nanHead = 0x7FF80000

// jsNull is a sentinel type representing JavaScript null (distinct from Go nil/undefined).
// Use this in callback args when the JS bridge expects null rather than undefined.
type jsNull struct{}

// JSNull is the null value for use in JS callback args.
var JSNull = jsNull{}

// loadValue reads a value reference from memory and returns the Go value.
// Used by standard Go ABI (reads 8 bytes from addr).
func (d *GoInstance) loadValue(addr int32) interface{} {
	bits := binary.LittleEndian.Uint64(d.mem.Data()[addr:])
	fv := math.Float64frombits(bits)
	if fv == 0 {
		return nil
	}
	if !math.IsNaN(fv) {
		return fv
	}
	id := binary.LittleEndian.Uint32(d.mem.Data()[addr:])
	return d.values[id]
}

// unboxValue extracts a Go value from a NaN-boxed uint64 ref.
// Used by TinyGo ABI.
func (d *GoInstance) unboxValue(vref uint64) interface{} {
	fv := math.Float64frombits(vref)
	if fv == 0 {
		return nil
	}
	if !math.IsNaN(fv) {
		return fv
	}
	id := uint32(vref)
	return d.values[id]
}

// storeValue NaN-boxes a Go value and writes 8 bytes to memory.
// Used by standard Go ABI.
func (d *GoInstance) storeValue(addr int32, v interface{}) {
	if vv, ok := v.(int64); ok {
		v = float64(vv)
	}
	if vv, ok := v.(int32); ok {
		v = float64(vv)
	}
	if vv, ok := v.(int); ok {
		v = float64(vv)
	}

	switch t := v.(type) {
	case float64:
		if t != 0 {
			if math.IsNaN(t) {
				binary.LittleEndian.PutUint32(d.mem.Data()[addr+4:], uint32(nanHead))
				binary.LittleEndian.PutUint32(d.mem.Data()[addr:], 0)
				return
			}
			bits := math.Float64bits(t)
			binary.LittleEndian.PutUint64(d.mem.Data()[addr:], bits)
			return
		}
	}

	if v == nil {
		binary.LittleEndian.PutUint64(d.mem.Data()[addr:], 0)
		return
	}

	if _, ok := v.(jsNull); ok {
		// Store as JS null: ref for values[2]
		binary.LittleEndian.PutUint64(d.mem.Data()[addr:], (uint64(nanHead)<<32)|2)
		return
	}

	ref := d.boxValue(v)
	binary.LittleEndian.PutUint64(d.mem.Data()[addr:], ref)
}

// valueKey returns a string key for the ids map, handling unhashable types
// by using pointer identity via reflect.
func valueKey(v interface{}) string {
	if s, ok := v.(string); ok {
		return "s:" + s
	}
	return fmt.Sprintf("p:%p", v)
}

// boxValue NaN-boxes a Go value into a uint64 ref.
func (d *GoInstance) boxValue(v interface{}) uint64 {
	switch t := v.(type) {
	case float64:
		if math.IsNaN(t) {
			return uint64(nanHead) << 32
		}
		if t == 0 {
			return (uint64(nanHead) << 32) | 1
		}
		return math.Float64bits(t)
	case nil:
		return (uint64(nanHead) << 32) | 2
	case bool:
		if t {
			return (uint64(nanHead) << 32) | 3
		}
		return (uint64(nanHead) << 32) | 4
	}

	// Convert numeric types to float64
	switch t := v.(type) {
	case int:
		return math.Float64bits(float64(t))
	case int32:
		return math.Float64bits(float64(t))
	case int64:
		return math.Float64bits(float64(t))
	case uint32:
		return math.Float64bits(float64(t))
	case uint64:
		return math.Float64bits(float64(t))
	}

	// Object/string/function — look up or allocate an id.
	// Functions are never deduplicated: Go's GC can reuse pointers for
	// different closures, so pointer-based identity is unreliable.
	_, isFunc := v.(func([]interface{}) interface{})

	var id uint64
	key := valueKey(v)
	if existingID, ok := d.ids[key]; ok && !isFunc {
		id = existingID
	} else {
		if len(d.idPool) > 0 {
			id = d.idPool[len(d.idPool)-1]
			d.idPool = d.idPool[:len(d.idPool)-1]
		} else {
			id = uint64(len(d.values))
			d.values = append(d.values, nil)
			d.goRefCounts = append(d.goRefCounts, 0)
		}
		d.values[id] = v
		d.goRefCounts[id] = 0
		d.ids[key] = id
	}
	d.goRefCounts[id]++

	var typeFlag uint64 = 1
	switch v.(type) {
	case string:
		typeFlag = 2
	case func([]interface{}) interface{}:
		typeFlag = 4
	}
	return id | ((uint64(nanHead) | typeFlag) << 32)
}

// loadString reads a Go string from memory (standard Go ABI: ptr at addr, len at addr+8).
func (d *GoInstance) loadString(addr int32) string {
	array := d.getInt64(addr)
	alen := d.getInt64(addr + 8)
	return string(d.mem.Data()[array : array+alen])
}

// loadStringDirect reads a string from memory given direct ptr and len (TinyGo ABI).
func (d *GoInstance) loadStringDirect(ptr, length int32) string {
	return string(d.mem.Data()[ptr : ptr+length])
}

// loadSlice reads a byte slice from memory (standard Go ABI: ptr at addr, len at addr+8).
func (d *GoInstance) loadSlice(addr int32) []byte {
	array := d.getInt64(addr)
	alen := d.getInt64(addr + 8)
	return d.mem.Data()[array : array+alen]
}

// loadSliceDirect reads a byte slice from memory given direct ptr and len (TinyGo ABI).
func (d *GoInstance) loadSliceDirect(ptr, length int32) []byte {
	return d.mem.Data()[ptr : ptr+length]
}

// loadSliceOfValues reads a slice of value references from memory (standard Go ABI).
func (d *GoInstance) loadSliceOfValues(addr int32) []interface{} {
	array := d.getInt64(addr)
	alen := d.getInt64(addr + 8)
	results := make([]interface{}, alen)
	for i := int64(0); i < alen; i++ {
		results[i] = d.loadValue(int32(array + i*8))
	}
	return results
}

// loadSliceOfValuesDirect reads a slice of value references (TinyGo ABI: direct ptr/len).
func (d *GoInstance) loadSliceOfValuesDirect(ptr, length int32) []interface{} {
	results := make([]interface{}, length)
	for i := int32(0); i < length; i++ {
		vref := binary.LittleEndian.Uint64(d.mem.Data()[ptr+i*8:])
		results[i] = d.unboxValue(vref)
	}
	return results
}

// --- Reflection helpers ---

func (d *GoInstance) reflectGet(v interface{}, key interface{}) interface{} {
	if v == nil {
		v = d.values[5]
	}

	// Handle []byte (Uint8Array) properties
	if buf, isBuf := v.([]byte); isBuf {
		if k, ok := key.(string); ok {
			switch k {
			case "byteLength", "length":
				return float64(len(buf))
			}
			return nil
		}
		if idx, ok := key.(float64); ok {
			i := int(idx)
			if i >= 0 && i < len(buf) {
				return float64(buf[i])
			}
		}
		return nil
	}

	if k, ok := key.(string); ok {
		m, mOk := v.(map[string]interface{})
		if !mOk {
			return nil
		}
		return m[k]
	}
	i := int(key.(int64))
	arr, ok := v.([]interface{})
	if !ok || i < 0 || i > len(arr)-1 {
		return nil
	}
	return arr[i]
}

func (d *GoInstance) reflectSet(v interface{}, key interface{}, value interface{}) {
	if v == nil {
		v = d.values[5]
	}
	if k, ok := key.(string); ok {
		m, mOk := v.(map[string]interface{})
		if !mOk {
			return
		}
		m[k] = value
		return
	}
	i := int(key.(int64))
	arr, ok := v.([]interface{})
	if !ok || i < 0 || i > len(arr)-1 {
		return
	}
	arr[i] = value
}

func (d *GoInstance) reflectDelete(v interface{}, key interface{}) {
	if v == nil {
		v = d.values[5]
	}
	if k, ok := key.(string); ok {
		delete(v.(map[string]interface{}), k)
		return
	}
	i := int(key.(int64))
	arr := v.([]interface{})
	if i < 0 || i > len(arr)-1 {
		return
	}
	copy(arr[i:], arr[i+1:])
	arr[len(arr)-1] = nil
	arr = arr[:len(arr)-1]
}

// reflectCall calls a method on a value (map lookup + function call).
func (d *GoInstance) reflectCall(v interface{}, method string, args []interface{}) (interface{}, bool) {
	rv := reflect.ValueOf(v)
	if !rv.IsValid() {
		return nil, false
	}
	m := rv.MapIndex(reflect.ValueOf(method))
	if !m.IsValid() {
		return nil, false
	}
	fn := m.Elem()
	result := fn.Call([]reflect.Value{reflect.ValueOf(args)})
	if len(result) > 0 {
		return result[0].Interface(), true
	}
	return nil, true
}

// reflectInvoke calls a value directly as a function.
func (d *GoInstance) reflectInvoke(v interface{}, args []interface{}) (interface{}, bool) {
	fn := reflect.ValueOf(v)
	if !fn.IsValid() || fn.Kind() != reflect.Func {
		return nil, false
	}
	result := fn.Call([]reflect.Value{reflect.ValueOf(args)})
	if len(result) > 0 {
		return result[0].Interface(), true
	}
	return nil, true
}

// reflectNew calls a constructor function (same as invoke for our purposes).
func (d *GoInstance) reflectNew(v interface{}, args []interface{}) (interface{}, bool) {
	return d.reflectInvoke(v, args)
}

// --- Initialization ---

func (d *GoInstance) initValues() {
	d.values = []interface{}{
		nil,   // 0: NaN
		0.0,   // 1: 0
		nil,   // 2: null
		true,  // 3: true
		false, // 4: false
		// 5: global object
		map[string]interface{}{
			"console": map[string]interface{}{
				"log": func(args []interface{}) interface{} {
					fmt.Fprintln(os.Stdout, args...)
					return nil
				},
				"error": func(args []interface{}) interface{} {
					fmt.Fprintln(os.Stderr, args...)
					return nil
				},
			},
			"Object": func([]interface{}) interface{} {
				return map[string]interface{}{}
			},
			"Array": func([]interface{}) interface{} {
				return []interface{}{}
			},
			"Uint8Array": func(args []interface{}) interface{} {
				if len(args) > 0 {
					switch v := args[0].(type) {
					case float64:
						return make([]byte, int(v))
					case []byte:
						buf := make([]byte, len(v))
						copy(buf, v)
						return buf
					}
				}
				return []byte{}
			},
			"crypto": map[string]interface{}{
				"getRandomValues": func(args []interface{}) interface{} {
					if len(args) > 0 {
						if buf, ok := args[0].([]byte); ok {
							rand.Read(buf)
						}
					}
					return nil
				},
			},
			"process": map[string]interface{}{},
			"fs": map[string]interface{}{
				"constants": map[string]interface{}{
					"O_WRONLY": float64(-1),
					"O_RDWR":   float64(-1),
					"O_CREAT":  float64(-1),
					"O_TRUNC":  float64(-1),
					"O_APPEND": float64(-1),
					"O_EXCL":   float64(-1),
				},
				"write": func(args []interface{}) interface{} {
					// fs.write(fd, buf, offset, length, position, callback)
					if len(args) < 6 {
						return nil
					}
					fd, _ := args[0].(float64)
					buf, _ := args[1].([]byte)
					offset := 0
					if v, ok := args[2].(float64); ok {
						offset = int(v)
					}
					length := 0
					if v, ok := args[3].(float64); ok {
						length = int(v)
					}
					callback, _ := args[5].(func([]interface{}) interface{})

					if buf != nil && length > 0 {
						end := offset + length
						if end > len(buf) {
							end = len(buf)
						}
						data := buf[offset:end]
						switch int(fd) {
						case 1:
							os.Stdout.Write(data)
						case 2:
							os.Stderr.Write(data)
						}
					}

					if callback != nil {
						callback([]interface{}{JSNull, float64(length)})
					}
					return nil
				},
				"read": func(args []interface{}) interface{} {
					// fs.read(fd, buf, offset, length, position, callback)
					if len(args) < 6 {
						return nil
					}
					fd, _ := args[0].(float64)
					buf, _ := args[1].([]byte)
					offset := 0
					if v, ok := args[2].(float64); ok {
						offset = int(v)
					}
					length := 0
					if v, ok := args[3].(float64); ok {
						length = int(v)
					}
					callback, _ := args[5].(func([]interface{}) interface{})

					n := 0
					if buf != nil && length > 0 && int(fd) == 0 {
						end := offset + length
						if end > len(buf) {
							end = len(buf)
						}
						var err error
						n, err = os.Stdin.Read(buf[offset:end])
						if err != nil && err != io.EOF && n == 0 && callback != nil {
							callback([]interface{}{err.Error(), float64(0)})
							return nil
						}
					}

					if callback != nil {
						callback([]interface{}{JSNull, float64(n)})
					}
					return nil
				},
				"chmod":  func(args []interface{}) interface{} { return nil },
				"chown":  func(args []interface{}) interface{} { return nil },
				"close":  func(args []interface{}) interface{} { return nil },
				"fchmod": func(args []interface{}) interface{} { return nil },
				"fchown": func(args []interface{}) interface{} { return nil },
				"fstat": func(args []interface{}) interface{} {
					// fs.fstat(fd, callback)
					if len(args) < 2 {
						return nil
					}
					fd := 0
					if v, ok := args[0].(float64); ok {
						fd = int(v)
					}
					callback, _ := args[len(args)-1].(func([]interface{}) interface{})

					// Build a stat-like object with the fields Go's setStat expects
					mode := float64(0)
					switch fd {
					case 0:
						// Stdin: check if the host's stdin is a terminal
						fi, err := os.Stdin.Stat()
						if err == nil && fi.Mode()&os.ModeCharDevice != 0 {
							mode = float64(0020000) // S_IFCHR
						} else {
							mode = float64(0010000) // S_IFIFO (pipe)
						}
					case 1, 2:
						mode = float64(0020000) // S_IFCHR for stdout/stderr
					}

					stat := map[string]interface{}{
						"dev":     float64(0),
						"ino":     float64(0),
						"mode":    mode,
						"nlink":   float64(1),
						"uid":     float64(0),
						"gid":     float64(0),
						"rdev":    float64(0),
						"size":    float64(0),
						"blksize": float64(4096),
						"blocks":  float64(0),
						"atimeMs": float64(0),
						"mtimeMs": float64(0),
						"ctimeMs": float64(0),
					}

					if callback != nil {
						callback([]interface{}{JSNull, stat})
					}
					return nil
				},
				"fsync":     func(args []interface{}) interface{} { return nil },
				"ftruncate": func(args []interface{}) interface{} { return nil },
				"lchown":    func(args []interface{}) interface{} { return nil },
				"link":      func(args []interface{}) interface{} { return nil },
				"lstat":     func(args []interface{}) interface{} { return nil },
				"mkdir":     func(args []interface{}) interface{} { return nil },
				"open":      func(args []interface{}) interface{} { return nil },
				"readdir":   func(args []interface{}) interface{} { return nil },
				"readlink":  func(args []interface{}) interface{} { return nil },
				"rename":    func(args []interface{}) interface{} { return nil },
				"rmdir":     func(args []interface{}) interface{} { return nil },
				"stat":      func(args []interface{}) interface{} { return nil },
				"symlink":   func(args []interface{}) interface{} { return nil },
				"truncate":  func(args []interface{}) interface{} { return nil },
				"unlink":    func(args []interface{}) interface{} { return nil },
				"utimes":    func(args []interface{}) interface{} { return nil },
			},
		},
		// 6: this (the Go runtime object)
		nil, // placeholder, set below
	}

	goObj := map[string]interface{}{
		"_pendingEvent": nil,
	}
	d.values[6] = goObj

	goObj["_makeFuncWrapper"] = func(args []interface{}) interface{} {
		id := args[0]
		return func(args []interface{}) interface{} {
			event := map[string]interface{}{
				"id":   id,
				"this": nil,
				"args": args,
			}
			goObj["_pendingEvent"] = event
			if d.resume != nil {
				_, err := d.resume()
				if err != nil {
					log.Print("resume error: ", err)
				}
			}
			return event["result"]
		}
	}

	d.goRefCounts = make([]int, len(d.values))
	d.ids = make(map[string]uint64)
	d.idPool = nil
	d.exited = false
}

// detectABI inspects module exports to determine the ABI type.
func detectABI(module *wasmer.Module) ABI {
	for _, exp := range module.Exports() {
		if exp.Name() == "getsp" {
			return ABIStdGo
		}
	}
	return ABITinyGo
}

// NewInstance creates an instance of GoRuntime from WASM bytes.
// It auto-detects whether the module was compiled with standard Go or TinyGo.
func NewInstance(b []byte, opts ...InstanceOption) (*GoInstance, error) {
	cfg := &instanceConfig{}
	for _, opt := range opts {
		opt(cfg)
	}

	engine := wasmer.NewEngine()
	store := wasmer.NewStore(engine)
	module, err := wasmer.NewModule(store, b)
	if err != nil {
		return nil, fmt.Errorf("failed to compile module: %w", err)
	}

	data := &GoInstance{}
	data.initValues()

	// Apply global modifiers (e.g. WithFetch installs fetch API)
	globals := data.values[5].(map[string]interface{})
	for _, mod := range cfg.globalModifiers {
		mod(globals)
	}

	data.abi = detectABI(module)

	importObject := wasmer.NewImportObject()

	switch data.abi {
	case ABIStdGo:
		importObject.Register("gojs", stdGoRuntime(store, data))
	case ABITinyGo:
		gojs := tinyGoRuntime(store, data)
		wasi := tinyGoWASI(store, data)
		importObject.Register("gojs", gojs)
		importObject.Register("wasi_snapshot_preview1", wasi)
		importObject.Register("env", gojs) // Go 1.20 compatibility
	}

	// Register additional namespaces
	for _, provider := range cfg.namespaces {
		name, externs := provider(store, data)
		importObject.Register(name, externs)
	}

	instance, err := wasmer.NewInstance(module, importObject)
	if err != nil {
		return nil, fmt.Errorf("failed to instantiate module: %w", err)
	}
	data.inst = instance

	switch data.abi {
	case ABIStdGo:
		return data.initStdGo(cfg)
	case ABITinyGo:
		return data.initTinyGo()
	}

	return data, nil
}

func (d *GoInstance) initStdGo(cfg *instanceConfig) (*GoInstance, error) {
	mem, err := d.inst.Exports.GetMemory("mem")
	if err != nil {
		return nil, fmt.Errorf("failed to get memory export 'mem': %w", err)
	}
	d.mem = mem

	// Set up argv/envp in linear memory
	args := cfg.args
	if len(args) == 0 {
		args = []string{"js"}
	}

	offset := 4096
	strPtr := func(str string) int {
		ptr := offset
		b := append([]byte(str), 0)
		copy(d.mem.Data()[offset:offset+len(b)], b)
		offset += len(b)
		if offset%8 != 0 {
			offset += 8 - (offset % 8)
		}
		return ptr
	}
	argPtrs := make([]int, 0, len(args)+2)
	for _, arg := range args {
		argPtrs = append(argPtrs, strPtr(arg))
	}
	argPtrs = append(argPtrs, 0, 0) // null terminators for argv and envp
	argvAddr := offset
	for _, ptr := range argPtrs {
		binary.LittleEndian.PutUint32(d.mem.Data()[offset:], uint32(ptr))
		binary.LittleEndian.PutUint32(d.mem.Data()[offset+4:], 0)
		offset += 8
	}

	getsp, err := d.inst.Exports.GetFunction("getsp")
	if err != nil {
		return nil, fmt.Errorf("failed to get 'getsp' export: %w", err)
	}
	d.getsp = getsp

	resume, err := d.inst.Exports.GetFunction("resume")
	if err != nil {
		return nil, fmt.Errorf("failed to get 'resume' export: %w", err)
	}
	d.resume = resume

	run, err := d.inst.Exports.GetFunction("run")
	if err != nil {
		return nil, fmt.Errorf("failed to get 'run' export: %w", err)
	}
	_, err = run(len(args), argvAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to call 'run': %w", err)
	}

	return d, nil
}

func (d *GoInstance) initTinyGo() (*GoInstance, error) {
	mem, err := d.inst.Exports.GetMemory("memory")
	if err != nil {
		return nil, fmt.Errorf("failed to get memory export 'memory': %w", err)
	}
	d.mem = mem

	resume, err := d.inst.Exports.GetFunction("resume")
	if err != nil {
		// TinyGo may not export resume
		d.resume = nil
	} else {
		d.resume = resume
	}

	start, err := d.inst.Exports.GetFunction("_start")
	if err != nil {
		return nil, fmt.Errorf("failed to get '_start' export: %w", err)
	}
	_, err = start()
	if err != nil && !d.exited {
		return nil, fmt.Errorf("failed to call '_start': %w", err)
	}

	return d, nil
}

// CompileGo compiles Go source in srcDir to a WASM binary using the standard Go compiler.
func CompileGo(srcDir string) ([]byte, error) {
	absSrcDir, err := filepath.Abs(srcDir)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve path: %w", err)
	}

	tmpFile := filepath.Join(absSrcDir, "gigwasm_build_tmp.wasm")
	cmd := exec.Command("go", "build", "-o", tmpFile, ".")
	cmd.Dir = absSrcDir
	cmd.Env = append(os.Environ(), "GOOS=js", "GOARCH=wasm")
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("go build failed: %w", err)
	}

	data, err := os.ReadFile(tmpFile)
	if err != nil {
		return nil, err
	}
	os.Remove(tmpFile)
	return data, nil
}

// CompileTinyGo compiles Go source in srcDir to a WASM binary using TinyGo.
func CompileTinyGo(srcDir string) ([]byte, error) {
	absSrcDir, err := filepath.Abs(srcDir)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve path: %w", err)
	}

	tmpFile := filepath.Join(absSrcDir, "gigwasm_build_tmp.wasm")
	cmd := exec.Command("tinygo", "build", "-target", "wasm", "-o", tmpFile, ".")
	cmd.Dir = absSrcDir
	cmd.Env = os.Environ()
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("tinygo build failed: %w", err)
	}

	data, err := os.ReadFile(tmpFile)
	if err != nil {
		return nil, err
	}
	os.Remove(tmpFile)
	return data, nil
}
