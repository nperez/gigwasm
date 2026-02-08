package main

import "syscall/js"

func main() {
	js.Global().Set("Add", js.FuncOf(func(this js.Value, args []js.Value) interface{} {
		return args[0].Float() + args[1].Float()
	}))

	select {}
}
