package gigwasm

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"reflect"
	"time"

	"github.com/wasmerio/wasmer-go/wasmer"
)

// stdGoRuntime returns the "gojs" import namespace for standard Go WASM modules.
// All functions take a single I32 (sp) parameter and return nothing.
func stdGoRuntime(store *wasmer.Store, data *GoInstance) map[string]wasmer.IntoExtern {
	spType := wasmer.NewFunctionType(wasmer.NewValueTypes(wasmer.I32), wasmer.NewValueTypes())

	return map[string]wasmer.IntoExtern{
		"debug": wasmer.NewFunction(store, spType,
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				sp := args[0].Unwrap()
				fmt.Println("DEBUG", sp)
				return []wasmer.Value{}, nil
			},
		),

		"runtime.resetMemoryDataView": wasmer.NewFunction(store, spType,
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				// no-op: wasmer memory doesn't change buffer identity
				return []wasmer.Value{}, nil
			},
		),

		"runtime.wasmExit": wasmer.NewFunction(store, spType,
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				sp := args[0].I32()
				data.exitCode = int(data.getInt32(sp + 8))
				data.exited = true
				return []wasmer.Value{}, nil
			},
		),

		"runtime.wasmWrite": wasmer.NewFunction(store, spType,
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				sp := args[0].I32()
				fd := data.getInt64(sp + 8)
				p := data.getInt64(sp + 16)
				n := data.getInt32(sp + 24)
				buf := data.mem.Data()[p : p+int64(n)]
				switch fd {
				case 1:
					os.Stdout.Write(buf)
				case 2:
					os.Stderr.Write(buf)
				}
				return []wasmer.Value{}, nil
			},
		),

		"runtime.nanotime1": wasmer.NewFunction(store, spType,
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				sp := args[0].I32()
				data.setInt64(sp+8, time.Now().UnixNano())
				return []wasmer.Value{}, nil
			},
		),

		"runtime.walltime": wasmer.NewFunction(store, spType,
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				sp := args[0].I32()
				msec := time.Now().UnixNano() / int64(time.Millisecond)
				data.setInt64(sp+8, msec/1000)
				data.setInt32(sp+16, (msec%1000)*1000000)
				return []wasmer.Value{}, nil
			},
		),

		"runtime.scheduleTimeoutEvent": wasmer.NewFunction(store, spType,
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				sp := args[0].I32()
				delayMs := data.getInt64(sp + 8)

				data.timerMu.Lock()
				id := data.nextTimerID
				data.nextTimerID++
				ctx, cancel := context.WithCancel(context.Background())
				entry := &timerEntry{
					id:     id,
					cancel: cancel,
					// callback nil = runtime timer (Path A)
				}
				data.scheduledTimers[id] = entry
				data.timerMu.Unlock()

				go data.timerGoroutine(id, float64(delayMs), ctx)

				data.setInt64(sp+16, int64(id))
				return []wasmer.Value{}, nil
			},
		),

		"runtime.clearTimeoutEvent": wasmer.NewFunction(store, spType,
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				sp := args[0].I32()
				id := int32(data.getInt64(sp + 8))

				data.timerMu.Lock()
				if entry, exists := data.scheduledTimers[id]; exists {
					entry.cancel()
					delete(data.scheduledTimers, id)
				}
				data.timerMu.Unlock()
				return []wasmer.Value{}, nil
			},
		),

		"runtime.getRandomData": wasmer.NewFunction(store, spType,
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				sp := args[0].I32()
				buf := data.loadSlice(sp + 8)
				rand.Read(buf)
				return []wasmer.Value{}, nil
			},
		),

		"syscall/js.finalizeRef": wasmer.NewFunction(store, spType,
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				sp := args[0].I32()
				id := uint32(data.getInt64(sp + 8))
				if id < 7 {
					// Don't finalize built-in values
					return []wasmer.Value{}, nil
				}
				data.goRefCounts[id]--
				if data.goRefCounts[id] == 0 {
					v := data.values[id]
					data.values[id] = nil
					delete(data.ids, valueKey(v))
					data.idPool = append(data.idPool, uint64(id))
				}
				return []wasmer.Value{}, nil
			},
		),

		"syscall/js.stringVal": wasmer.NewFunction(store, spType,
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				sp := args[0].I32()
				str := data.loadString(sp + 8)
				data.storeValue(sp+24, str)
				return []wasmer.Value{}, nil
			},
		),

		"syscall/js.valueGet": wasmer.NewFunction(store, spType,
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				sp := args[0].I32()
				result := data.reflectGet(data.loadValue(sp+8), data.loadString(sp+16))
				if v, err := data.getsp(); err == nil {
					sp = v.(int32)
				}
				data.storeValue(sp+32, result)
				return []wasmer.Value{}, nil
			},
		),

		"syscall/js.valueSet": wasmer.NewFunction(store, spType,
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				sp := args[0].I32()
				data.reflectSet(data.loadValue(sp+8), data.loadString(sp+16), data.loadValue(sp+32))
				return []wasmer.Value{}, nil
			},
		),

		"syscall/js.valueDelete": wasmer.NewFunction(store, spType,
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				sp := args[0].I32()
				data.reflectDelete(data.loadValue(sp+8), data.loadString(sp+16))
				return []wasmer.Value{}, nil
			},
		),

		"syscall/js.valueIndex": wasmer.NewFunction(store, spType,
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				sp := args[0].I32()
				data.storeValue(sp+24, data.reflectGet(data.loadValue(sp+8), data.getInt64(sp+16)))
				return []wasmer.Value{}, nil
			},
		),

		"syscall/js.valueSetIndex": wasmer.NewFunction(store, spType,
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				sp := args[0].I32()
				data.reflectSet(data.loadValue(sp+8), data.getInt64(sp+16), data.loadValue(sp+24))
				return []wasmer.Value{}, nil
			},
		),

		"syscall/js.valueCall": wasmer.NewFunction(store, spType,
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				sp := args[0].I32()
				v := data.loadValue(sp + 8)
				method := data.loadString(sp + 16)
				callArgs := data.loadSliceOfValues(sp + 32)
				result, ok := data.reflectCall(v, method, callArgs)
				if v2, err := data.getsp(); err == nil {
					sp = v2.(int32)
				}
				if ok {
					data.storeValue(sp+56, result)
					data.setUint8(sp+64, 1)
				} else {
					data.storeValue(sp+56, nil)
					data.setUint8(sp+64, 0)
				}
				return []wasmer.Value{}, nil
			},
		),

		"syscall/js.valueInvoke": wasmer.NewFunction(store, spType,
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				sp := args[0].I32()
				v := data.loadValue(sp + 8)
				callArgs := data.loadSliceOfValues(sp + 16)
				result, ok := data.reflectInvoke(v, callArgs)
				if v2, err := data.getsp(); err == nil {
					sp = v2.(int32)
				}
				if ok {
					data.storeValue(sp+40, result)
					data.setUint8(sp+48, 1)
				} else {
					data.storeValue(sp+40, nil)
					data.setUint8(sp+48, 0)
				}
				return []wasmer.Value{}, nil
			},
		),

		"syscall/js.valueNew": wasmer.NewFunction(store, spType,
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				sp := args[0].I32()
				v := data.loadValue(sp + 8)
				callArgs := data.loadSliceOfValues(sp + 16)
				result, ok := data.reflectNew(v, callArgs)
				if v2, err := data.getsp(); err == nil {
					sp = v2.(int32)
				}
				if ok {
					data.storeValue(sp+40, result)
					data.setUint8(sp+48, 1)
				} else {
					data.storeValue(sp+40, nil)
					data.setUint8(sp+48, 0)
				}
				return []wasmer.Value{}, nil
			},
		),

		"syscall/js.valueLength": wasmer.NewFunction(store, spType,
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				sp := args[0].I32()
				v := data.loadValue(sp + 8)
				length := int64(0)
				switch t := v.(type) {
				case []interface{}:
					length = int64(len(t))
				case []byte:
					length = int64(len(t))
				case string:
					length = int64(len(t))
				}
				data.setInt64(sp+16, length)
				return []wasmer.Value{}, nil
			},
		),

		"syscall/js.valuePrepareString": wasmer.NewFunction(store, spType,
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				sp := args[0].I32()
				v := data.loadValue(sp + 8)
				str := fmt.Sprint(v)
				data.storeValue(sp+16, str)
				data.setInt64(sp+24, int64(len(str)))
				return []wasmer.Value{}, nil
			},
		),

		"syscall/js.valueLoadString": wasmer.NewFunction(store, spType,
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				sp := args[0].I32()
				v := data.loadValue(sp + 8)
				str := fmt.Sprint(v)
				dst := data.loadSlice(sp + 16)
				copy(dst, []byte(str))
				return []wasmer.Value{}, nil
			},
		),

		"syscall/js.valueInstanceOf": wasmer.NewFunction(store, spType,
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				sp := args[0].I32()
				// We don't have real JS instanceof; always return false
				data.setUint8(sp+24, 0)
				return []wasmer.Value{}, nil
			},
		),

		"syscall/js.copyBytesToGo": wasmer.NewFunction(store, spType,
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				sp := args[0].I32()
				dst := data.loadSlice(sp + 8)
				src := data.loadValue(sp + 32)
				srcBytes, ok := src.([]byte)
				if !ok || len(dst) == 0 {
					data.setUint8(sp+48, 0)
					return []wasmer.Value{}, nil
				}
				n := copy(dst, srcBytes)
				data.setInt64(sp+40, int64(n))
				data.setUint8(sp+48, 1)
				return []wasmer.Value{}, nil
			},
		),

		"syscall/js.copyBytesToJS": wasmer.NewFunction(store, spType,
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				sp := args[0].I32()
				dst := data.loadValue(sp + 8)
				src := data.loadSlice(sp + 16)
				dstBytes, ok := dst.([]byte)
				if !ok {
					data.setUint8(sp+48, 0)
					return []wasmer.Value{}, nil
				}
				n := copy(dstBytes, src)
				data.setInt64(sp+40, int64(n))
				data.setUint8(sp+48, 1)
				return []wasmer.Value{}, nil
			},
		),
	}
}

// Ensure reflect is used (for reflectCall in the runtime functions).
var _ = reflect.ValueOf
