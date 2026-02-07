package gigwasm

import (
	"fmt"
	"io"
	"net/http"
	"strings"
)

// WithFetch installs the WHATWG Fetch API in the global object using
// http.DefaultClient for all requests. This enables Go's net/http to work
// in WASM modules compiled with GOOS=js GOARCH=wasm.
func WithFetch() InstanceOption {
	return WithFetchClient(http.DefaultClient)
}

// WithFetchClient installs the WHATWG Fetch API using the provided HTTP client.
func WithFetchClient(client *http.Client) InstanceOption {
	return withGlobalModifier(func(global map[string]interface{}) {
		installFetchAPI(global, client)
	})
}

// installFetchAPI adds `fetch` and `Headers` to the global object.
func installFetchAPI(global map[string]interface{}, client *http.Client) {
	global["fetch"] = func(args []interface{}) interface{} {
		return doFetch(client, args)
	}

	global["Headers"] = func(args []interface{}) interface{} {
		return newHeaders()
	}
}

// doFetch performs a synchronous HTTP request and returns a resolved Promise.
func doFetch(client *http.Client, args []interface{}) interface{} {
	if len(args) < 1 {
		return newRejectedPromise(newJSError("fetch: missing URL argument"))
	}

	url, ok := args[0].(string)
	if !ok {
		return newRejectedPromise(newJSError("fetch: URL must be a string"))
	}

	// Extract options from second argument
	method := "GET"
	var body io.Reader
	var headers map[string]interface{}

	if len(args) > 1 {
		if opts, ok := args[1].(map[string]interface{}); ok {
			if m, ok := opts["method"].(string); ok {
				method = m
			}
			if h, ok := opts["headers"].(map[string]interface{}); ok {
				headers = h
			}
			if b, ok := opts["body"].([]byte); ok && len(b) > 0 {
				body = strings.NewReader(string(b))
			}
		}
	}

	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return newRejectedPromise(newJSError(fmt.Sprintf("fetch: %v", err)))
	}

	applyHeaders(req, headers)

	resp, err := client.Do(req)
	if err != nil {
		return newRejectedPromise(newJSError(fmt.Sprintf("fetch: %v", err)))
	}

	respBody, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return newRejectedPromise(newJSError(fmt.Sprintf("fetch: error reading body: %v", err)))
	}

	return newResolvedPromise(buildResponse(resp, respBody))
}

// applyHeaders extracts headers from a Headers object into an http.Request.
func applyHeaders(req *http.Request, hdrs map[string]interface{}) {
	if hdrs == nil {
		return
	}

	// Headers are stored in _store as map[string][]string
	store, ok := hdrs["_store"].(map[string][]string)
	if !ok {
		return
	}
	for key, values := range store {
		for _, v := range values {
			req.Header.Add(key, v)
		}
	}
}

// buildResponse creates a Response-like object from an http.Response and body bytes.
func buildResponse(resp *http.Response, body []byte) map[string]interface{} {
	// Build response headers
	respHeaders := newHeaders()
	store := respHeaders["_store"].(map[string][]string)
	order := respHeaders["_order"].([]string)
	for key, values := range resp.Header {
		lk := strings.ToLower(key)
		store[lk] = append(store[lk], values...)
		order = append(order, lk)
	}
	respHeaders["_order"] = order

	result := map[string]interface{}{
		"status":  float64(resp.StatusCode),
		"headers": respHeaders,
		"body":    newReadableStreamBody(body),
		"arrayBuffer": func(args []interface{}) interface{} {
			// Return a resolved promise with the body bytes (as []byte, which is our ArrayBuffer)
			return newResolvedPromise(body)
		},
	}
	return result
}

// --- Headers ---

// newHeaders creates a JS Headers-like object.
func newHeaders() map[string]interface{} {
	store := map[string][]string{}
	order := []string{} // track insertion order for iteration

	h := map[string]interface{}{
		"_store": store,
		"_order": order,
	}

	h["append"] = func(args []interface{}) interface{} {
		if len(args) < 2 {
			return nil
		}
		key, _ := args[0].(string)
		value, _ := args[1].(string)
		lk := strings.ToLower(key)
		s := h["_store"].(map[string][]string)
		o := h["_order"].([]string)
		s[lk] = append(s[lk], value)
		h["_store"] = s
		// Track order (only add once per key)
		found := false
		for _, k := range o {
			if k == lk {
				found = true
				break
			}
		}
		if !found {
			o = append(o, lk)
			h["_order"] = o
		}
		return nil
	}

	h["set"] = func(args []interface{}) interface{} {
		if len(args) < 2 {
			return nil
		}
		key, _ := args[0].(string)
		value, _ := args[1].(string)
		lk := strings.ToLower(key)
		s := h["_store"].(map[string][]string)
		o := h["_order"].([]string)
		s[lk] = []string{value}
		h["_store"] = s
		found := false
		for _, k := range o {
			if k == lk {
				found = true
				break
			}
		}
		if !found {
			o = append(o, lk)
			h["_order"] = o
		}
		return nil
	}

	h["get"] = func(args []interface{}) interface{} {
		if len(args) < 1 {
			return nil
		}
		key, _ := args[0].(string)
		lk := strings.ToLower(key)
		s := h["_store"].(map[string][]string)
		vals, ok := s[lk]
		if !ok || len(vals) == 0 {
			return nil
		}
		return strings.Join(vals, ", ")
	}

	h["entries"] = func(args []interface{}) interface{} {
		s := h["_store"].(map[string][]string)
		o := h["_order"].([]string)
		return newHeadersIterator(s, o)
	}

	return h
}

// newHeadersIterator creates an iterator over headers, yielding [key, value] pairs.
func newHeadersIterator(store map[string][]string, order []string) map[string]interface{} {
	// Build flat list of [key, value] pairs
	type pair struct{ key, val string }
	var pairs []pair
	for _, key := range order {
		for _, val := range store[key] {
			pairs = append(pairs, pair{key, val})
		}
	}

	idx := 0
	iter := map[string]interface{}{}

	iter["next"] = func(args []interface{}) interface{} {
		if idx >= len(pairs) {
			return map[string]interface{}{
				"done":  true,
				"value": nil,
			}
		}
		p := pairs[idx]
		idx++
		return map[string]interface{}{
			"done":  false,
			"value": []interface{}{p.key, p.val},
		}
	}

	return iter
}

// --- Promise ---

// newResolvedPromise creates a Promise-like object that resolves immediately with value.
func newResolvedPromise(value interface{}) map[string]interface{} {
	p := map[string]interface{}{}

	p["then"] = func(args []interface{}) interface{} {
		if len(args) > 0 {
			if success, ok := args[0].(func([]interface{}) interface{}); ok {
				return success([]interface{}{value})
			}
		}
		return nil
	}

	return p
}

// newRejectedPromise creates a Promise-like object that rejects immediately with err.
func newRejectedPromise(errVal interface{}) map[string]interface{} {
	p := map[string]interface{}{}

	p["then"] = func(args []interface{}) interface{} {
		if len(args) > 1 {
			if failure, ok := args[1].(func([]interface{}) interface{}); ok {
				return failure([]interface{}{errVal})
			}
		}
		return nil
	}

	return p
}

// --- ReadableStream ---

// newReadableStreamBody creates a body object with getReader() that delivers data.
func newReadableStreamBody(data []byte) map[string]interface{} {
	body := map[string]interface{}{}

	body["getReader"] = func(args []interface{}) interface{} {
		return newStreamReader(data)
	}

	return body
}

// newStreamReader creates a reader that delivers all data in one chunk, then done.
func newStreamReader(data []byte) map[string]interface{} {
	delivered := false
	reader := map[string]interface{}{}

	reader["read"] = func(args []interface{}) interface{} {
		if !delivered {
			delivered = true
			chunk := make([]byte, len(data))
			copy(chunk, data)
			result := map[string]interface{}{
				"done":  false,
				"value": chunk,
			}
			return newResolvedPromise(result)
		}
		// All data delivered
		result := map[string]interface{}{
			"done":  true,
			"value": nil,
		}
		return newResolvedPromise(result)
	}

	reader["cancel"] = func(args []interface{}) interface{} {
		delivered = true
		return nil
	}

	return reader
}

// --- JS Error ---

// newJSError creates a JS Error-like object with a message and toString() method.
func newJSError(msg string) map[string]interface{} {
	e := map[string]interface{}{
		"message": msg,
	}
	e["toString"] = func(args []interface{}) interface{} {
		return "Error: " + msg
	}
	return e
}
