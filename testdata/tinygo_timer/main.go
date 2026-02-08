package main

import (
	"syscall/js"
	"time"
)

func main() {
	start := time.Now()

	time.Sleep(50 * time.Millisecond)

	elapsed := time.Since(start)

	js.Global().Set("TimerElapsedMs", elapsed.Milliseconds())
	js.Global().Set("TimerDone", true)
}
