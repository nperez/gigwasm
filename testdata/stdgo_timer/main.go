package main

import (
	"syscall/js"
	"time"
)

func main() {
	start := time.Now()

	// Sleep for a short duration to exercise the timer infrastructure
	time.Sleep(50 * time.Millisecond)

	elapsed := time.Since(start)

	// Export results so the host can verify
	js.Global().Set("TimerElapsedMs", elapsed.Milliseconds())
	js.Global().Set("TimerDone", true)
}
