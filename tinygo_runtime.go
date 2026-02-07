package gigwasm

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/wasmerio/wasmer-go/wasmer"
)

// tinyGoRuntime returns the "gojs" import namespace for TinyGo WASM modules.
// TinyGo functions have individual typed parameters (not a single sp).
func tinyGoRuntime(store *wasmer.Store, data *GoInstance) map[string]wasmer.IntoExtern {
	timeOrigin := time.Now()

	return map[string]wasmer.IntoExtern{
		// func runtime.ticks() float64
		"runtime.ticks": wasmer.NewFunction(
			store,
			wasmer.NewFunctionType(wasmer.NewValueTypes(), wasmer.NewValueTypes(wasmer.F64)),
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				elapsed := time.Since(timeOrigin)
				ms := float64(elapsed.Nanoseconds()) / 1e6
				return []wasmer.Value{wasmer.NewF64(ms)}, nil
			},
		),

		// func runtime.sleepTicks(timeout float64)
		"runtime.sleepTicks": wasmer.NewFunction(
			store,
			wasmer.NewFunctionType(wasmer.NewValueTypes(wasmer.F64), wasmer.NewValueTypes()),
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				timeout := args[0].F64()
				go func() {
					time.Sleep(time.Duration(timeout) * time.Millisecond)
					fn, err := data.inst.Exports.GetFunction("go_scheduler")
					if err == nil && fn != nil {
						fn()
					}
				}()
				return []wasmer.Value{}, nil
			},
		),

		// func syscall/js.finalizeRef(v_ref int64)
		"syscall/js.finalizeRef": wasmer.NewFunction(
			store,
			wasmer.NewFunctionType(wasmer.NewValueTypes(wasmer.I64), wasmer.NewValueTypes()),
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				// TinyGo doesn't support finalizers, so this is a no-op
				return []wasmer.Value{}, nil
			},
		),

		// func syscall/js.stringVal(ptr int32, len int32) ref
		"syscall/js.stringVal": wasmer.NewFunction(
			store,
			wasmer.NewFunctionType(
				wasmer.NewValueTypes(wasmer.I32, wasmer.I32),
				wasmer.NewValueTypes(wasmer.I64),
			),
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				ptr := args[0].I32()
				length := args[1].I32()
				s := data.loadStringDirect(ptr, length)
				ref := data.boxValue(s)
				return []wasmer.Value{wasmer.NewI64(int64(ref))}, nil
			},
		),

		// func syscall/js.valueGet(v_ref int64, p_ptr int32, p_len int32) ref
		"syscall/js.valueGet": wasmer.NewFunction(
			store,
			wasmer.NewFunctionType(
				wasmer.NewValueTypes(wasmer.I64, wasmer.I32, wasmer.I32),
				wasmer.NewValueTypes(wasmer.I64),
			),
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				vRef := uint64(args[0].I64())
				pPtr := args[1].I32()
				pLen := args[2].I32()
				v := data.unboxValue(vRef)
				prop := data.loadStringDirect(pPtr, pLen)
				result := data.reflectGet(v, prop)
				ref := data.boxValue(result)
				return []wasmer.Value{wasmer.NewI64(int64(ref))}, nil
			},
		),

		// func syscall/js.valueSet(v_ref int64, p_ptr int32, p_len int32, x_ref int64)
		"syscall/js.valueSet": wasmer.NewFunction(
			store,
			wasmer.NewFunctionType(
				wasmer.NewValueTypes(wasmer.I64, wasmer.I32, wasmer.I32, wasmer.I64),
				wasmer.NewValueTypes(),
			),
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				vRef := uint64(args[0].I64())
				pPtr := args[1].I32()
				pLen := args[2].I32()
				xRef := uint64(args[3].I64())
				v := data.unboxValue(vRef)
				p := data.loadStringDirect(pPtr, pLen)
				x := data.unboxValue(xRef)
				data.reflectSet(v, p, x)
				return []wasmer.Value{}, nil
			},
		),

		// func syscall/js.valueDelete(v_ref int64, p_ptr int32, p_len int32)
		"syscall/js.valueDelete": wasmer.NewFunction(
			store,
			wasmer.NewFunctionType(
				wasmer.NewValueTypes(wasmer.I64, wasmer.I32, wasmer.I32),
				wasmer.NewValueTypes(),
			),
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				vRef := uint64(args[0].I64())
				pPtr := args[1].I32()
				pLen := args[2].I32()
				v := data.unboxValue(vRef)
				p := data.loadStringDirect(pPtr, pLen)
				data.reflectDelete(v, p)
				return []wasmer.Value{}, nil
			},
		),

		// func syscall/js.valueIndex(v_ref int64, i int32) ref
		"syscall/js.valueIndex": wasmer.NewFunction(
			store,
			wasmer.NewFunctionType(
				wasmer.NewValueTypes(wasmer.I64, wasmer.I32),
				wasmer.NewValueTypes(wasmer.I64),
			),
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				vRef := uint64(args[0].I64())
				i := int64(args[1].I32())
				v := data.unboxValue(vRef)
				result := data.reflectGet(v, i)
				ref := data.boxValue(result)
				return []wasmer.Value{wasmer.NewI64(int64(ref))}, nil
			},
		),

		// func syscall/js.valueSetIndex(v_ref int64, i int64, x_ref int64)
		"syscall/js.valueSetIndex": wasmer.NewFunction(
			store,
			wasmer.NewFunctionType(
				wasmer.NewValueTypes(wasmer.I64, wasmer.I64, wasmer.I64),
				wasmer.NewValueTypes(),
			),
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				vRef := uint64(args[0].I64())
				i := args[1].I64()
				xRef := uint64(args[2].I64())
				v := data.unboxValue(vRef)
				x := data.unboxValue(xRef)
				data.reflectSet(v, i, x)
				return []wasmer.Value{}, nil
			},
		),

		// func syscall/js.valueCall(ret_addr int32, v_ref int64, m_ptr int32, m_len int32, args_ptr int32, args_len int32, args_cap int32)
		"syscall/js.valueCall": wasmer.NewFunction(
			store,
			wasmer.NewFunctionType(
				wasmer.NewValueTypes(wasmer.I32, wasmer.I64, wasmer.I32, wasmer.I32, wasmer.I32, wasmer.I32, wasmer.I32),
				wasmer.NewValueTypes(),
			),
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				retAddr := args[0].I32()
				vRef := uint64(args[1].I64())
				mPtr := args[2].I32()
				mLen := args[3].I32()
				argsPtr := args[4].I32()
				argsLen := args[5].I32()

				v := data.unboxValue(vRef)
				method := data.loadStringDirect(mPtr, mLen)
				callArgs := data.loadSliceOfValuesDirect(argsPtr, argsLen)
				result, ok := data.reflectCall(v, method, callArgs)
				if ok {
					data.storeValue(retAddr, result)
					data.setUint8(retAddr+8, 1)
				} else {
					data.storeValue(retAddr, nil)
					data.setUint8(retAddr+8, 0)
				}
				return []wasmer.Value{}, nil
			},
		),

		// func syscall/js.valueInvoke(ret_addr int32, v_ref int64, args_ptr int32, args_len int32, args_cap int32)
		"syscall/js.valueInvoke": wasmer.NewFunction(
			store,
			wasmer.NewFunctionType(
				wasmer.NewValueTypes(wasmer.I32, wasmer.I64, wasmer.I32, wasmer.I32, wasmer.I32),
				wasmer.NewValueTypes(),
			),
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				retAddr := args[0].I32()
				vRef := uint64(args[1].I64())
				argsPtr := args[2].I32()
				argsLen := args[3].I32()

				v := data.unboxValue(vRef)
				callArgs := data.loadSliceOfValuesDirect(argsPtr, argsLen)
				result, ok := data.reflectInvoke(v, callArgs)
				if ok {
					data.storeValue(retAddr, result)
					data.setUint8(retAddr+8, 1)
				} else {
					data.storeValue(retAddr, nil)
					data.setUint8(retAddr+8, 0)
				}
				return []wasmer.Value{}, nil
			},
		),

		// func syscall/js.valueNew(ret_addr int32, v_ref int64, args_ptr int32, args_len int32, args_cap int32)
		"syscall/js.valueNew": wasmer.NewFunction(
			store,
			wasmer.NewFunctionType(
				wasmer.NewValueTypes(wasmer.I32, wasmer.I64, wasmer.I32, wasmer.I32, wasmer.I32),
				wasmer.NewValueTypes(),
			),
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				retAddr := args[0].I32()
				vRef := uint64(args[1].I64())
				argsPtr := args[2].I32()
				argsLen := args[3].I32()

				v := data.unboxValue(vRef)
				callArgs := data.loadSliceOfValuesDirect(argsPtr, argsLen)
				result, ok := data.reflectNew(v, callArgs)
				if ok {
					data.storeValue(retAddr, result)
					data.setUint8(retAddr+8, 1)
				} else {
					data.storeValue(retAddr, nil)
					data.setUint8(retAddr+8, 0)
				}
				return []wasmer.Value{}, nil
			},
		),

		// func syscall/js.valueLength(v_ref int64) int32
		"syscall/js.valueLength": wasmer.NewFunction(
			store,
			wasmer.NewFunctionType(
				wasmer.NewValueTypes(wasmer.I64),
				wasmer.NewValueTypes(wasmer.I32),
			),
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				vRef := uint64(args[0].I64())
				v := data.unboxValue(vRef)
				length := int32(0)
				switch t := v.(type) {
				case []interface{}:
					length = int32(len(t))
				case []byte:
					length = int32(len(t))
				case string:
					length = int32(len(t))
				}
				return []wasmer.Value{wasmer.NewI32(length)}, nil
			},
		),

		// func syscall/js.valuePrepareString(ret_addr int32, v_ref int64)
		"syscall/js.valuePrepareString": wasmer.NewFunction(
			store,
			wasmer.NewFunctionType(
				wasmer.NewValueTypes(wasmer.I32, wasmer.I64),
				wasmer.NewValueTypes(),
			),
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				retAddr := args[0].I32()
				vRef := uint64(args[1].I64())
				v := data.unboxValue(vRef)
				str := fmt.Sprint(v)
				encoded := []byte(str)
				data.storeValue(retAddr, encoded)
				data.setInt32(retAddr+8, int64(len(encoded)))
				return []wasmer.Value{}, nil
			},
		),

		// func syscall/js.valueLoadString(v_ref int64, ptr int32, len int32, cap int32)
		"syscall/js.valueLoadString": wasmer.NewFunction(
			store,
			wasmer.NewFunctionType(
				wasmer.NewValueTypes(wasmer.I64, wasmer.I32, wasmer.I32, wasmer.I32),
				wasmer.NewValueTypes(),
			),
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				vRef := uint64(args[0].I64())
				ptr := args[1].I32()
				length := args[2].I32()
				v := data.unboxValue(vRef)
				if buf, ok := v.([]byte); ok {
					dst := data.loadSliceDirect(ptr, length)
					copy(dst, buf)
				}
				return []wasmer.Value{}, nil
			},
		),

		// func syscall/js.valueInstanceOf(v_ref int64, t_ref int64) int32
		"syscall/js.valueInstanceOf": wasmer.NewFunction(
			store,
			wasmer.NewFunctionType(
				wasmer.NewValueTypes(wasmer.I64, wasmer.I64),
				wasmer.NewValueTypes(wasmer.I32),
			),
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				// No real instanceof support; always false
				return []wasmer.Value{wasmer.NewI32(0)}, nil
			},
		),

		// func syscall/js.copyBytesToGo(ret_addr int32, dest_addr int32, dest_len int32, dest_cap int32, src_ref int64)
		"syscall/js.copyBytesToGo": wasmer.NewFunction(
			store,
			wasmer.NewFunctionType(
				wasmer.NewValueTypes(wasmer.I32, wasmer.I32, wasmer.I32, wasmer.I32, wasmer.I64),
				wasmer.NewValueTypes(),
			),
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				retAddr := args[0].I32()
				destAddr := args[1].I32()
				destLen := args[2].I32()
				srcRef := uint64(args[4].I64())

				dst := data.loadSliceDirect(destAddr, destLen)
				src := data.unboxValue(srcRef)
				srcBytes, ok := src.([]byte)
				if !ok {
					data.setUint8(retAddr+4, 0)
					return []wasmer.Value{}, nil
				}
				n := copy(dst, srcBytes)
				data.setUint32(retAddr, uint32(n))
				data.setUint8(retAddr+4, 1)
				return []wasmer.Value{}, nil
			},
		),

		// func syscall/js.copyBytesToJS(ret_addr int32, dst_ref int64, src_addr int32, src_len int32, src_cap int32)
		"syscall/js.copyBytesToJS": wasmer.NewFunction(
			store,
			wasmer.NewFunctionType(
				wasmer.NewValueTypes(wasmer.I32, wasmer.I64, wasmer.I32, wasmer.I32, wasmer.I32),
				wasmer.NewValueTypes(),
			),
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				retAddr := args[0].I32()
				dstRef := uint64(args[1].I64())
				srcAddr := args[2].I32()
				srcLen := args[3].I32()

				dst := data.unboxValue(dstRef)
				src := data.loadSliceDirect(srcAddr, srcLen)
				dstBytes, ok := dst.([]byte)
				if !ok {
					data.setUint8(retAddr+4, 0)
					return []wasmer.Value{}, nil
				}
				n := copy(dstBytes, src)
				data.setUint32(retAddr, uint32(n))
				data.setUint8(retAddr+4, 1)
				return []wasmer.Value{}, nil
			},
		),
	}
}

// tinyGoWASI returns the "wasi_snapshot_preview1" import namespace for TinyGo.
func tinyGoWASI(store *wasmer.Store, data *GoInstance) map[string]wasmer.IntoExtern {
	var logLine []byte

	return map[string]wasmer.IntoExtern{
		"fd_write": wasmer.NewFunction(
			store,
			wasmer.NewFunctionType(
				wasmer.NewValueTypes(wasmer.I32, wasmer.I32, wasmer.I32, wasmer.I32),
				wasmer.NewValueTypes(wasmer.I32),
			),
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				fd := args[0].I32()
				iovsPtr := args[1].I32()
				iovsLen := args[2].I32()
				nwrittenPtr := args[3].I32()

				var nwritten int32
				if fd == 1 || fd == 2 {
					for i := int32(0); i < iovsLen; i++ {
						iovPtr := iovsPtr + i*8
						ptr := data.getInt32(iovPtr)
						length := data.getInt32(iovPtr + 4)
						nwritten += length
						for j := int32(0); j < length; j++ {
							c := data.getUint8(ptr + j)
							if c == 13 {
								// CR: ignore
							} else if c == 10 {
								// LF: flush line
								line := string(logLine)
								logLine = logLine[:0]
								if fd == 1 {
									fmt.Fprintln(os.Stdout, line)
								} else {
									fmt.Fprintln(os.Stderr, line)
								}
							} else {
								logLine = append(logLine, c)
							}
						}
					}
				} else {
					log.Printf("fd_write: unsupported fd %d", fd)
				}
				binary.LittleEndian.PutUint32(data.mem.Data()[nwrittenPtr:], uint32(nwritten))
				return []wasmer.Value{wasmer.NewI32(0)}, nil
			},
		),

		"fd_close": wasmer.NewFunction(
			store,
			wasmer.NewFunctionType(wasmer.NewValueTypes(wasmer.I32), wasmer.NewValueTypes(wasmer.I32)),
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				return []wasmer.Value{wasmer.NewI32(0)}, nil
			},
		),

		"fd_fdstat_get": wasmer.NewFunction(
			store,
			wasmer.NewFunctionType(wasmer.NewValueTypes(wasmer.I32, wasmer.I32), wasmer.NewValueTypes(wasmer.I32)),
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				return []wasmer.Value{wasmer.NewI32(0)}, nil
			},
		),

		"fd_seek": wasmer.NewFunction(
			store,
			wasmer.NewFunctionType(
				wasmer.NewValueTypes(wasmer.I32, wasmer.I64, wasmer.I32, wasmer.I32),
				wasmer.NewValueTypes(wasmer.I32),
			),
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				return []wasmer.Value{wasmer.NewI32(0)}, nil
			},
		),

		"proc_exit": wasmer.NewFunction(
			store,
			wasmer.NewFunctionType(wasmer.NewValueTypes(wasmer.I32), wasmer.NewValueTypes()),
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				code := args[0].I32()
				data.exited = true
				if code != 0 {
					fmt.Fprintf(os.Stderr, "wasm proc_exit with code %d\n", code)
				}
				return []wasmer.Value{}, nil
			},
		),

		"random_get": wasmer.NewFunction(
			store,
			wasmer.NewFunctionType(
				wasmer.NewValueTypes(wasmer.I32, wasmer.I32),
				wasmer.NewValueTypes(wasmer.I32),
			),
			func(args []wasmer.Value) ([]wasmer.Value, error) {
				bufPtr := args[0].I32()
				bufLen := args[1].I32()
				buf := data.loadSliceDirect(bufPtr, bufLen)
				rand.Read(buf)
				return []wasmer.Value{wasmer.NewI32(0)}, nil
			},
		),
	}
}

// Ensure imports are used.
var _ = bytes.NewBuffer
