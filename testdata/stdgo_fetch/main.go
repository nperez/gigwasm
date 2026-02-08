package main

import (
	"io"
	"net/http"
	"os"
	"syscall/js"
)

func main() {
	url := os.Args[1]
	resp, err := http.Get(url)
	if err != nil {
		js.Global().Set("FetchError", err.Error())
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		js.Global().Set("FetchError", err.Error())
		return
	}

	js.Global().Set("FetchStatus", resp.StatusCode)
	js.Global().Set("FetchBody", string(body))
}
