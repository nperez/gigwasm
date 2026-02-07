package gigwasm

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"
)

func TestCompileAndRunStdGo(t *testing.T) {
	// Check that go is available
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go compiler not found in PATH")
	}

	wasmBytes, err := CompileGo("./testdata/stdgo")
	if err != nil {
		t.Fatalf("CompileGo failed: %v", err)
	}

	if len(wasmBytes) == 0 {
		t.Fatal("CompileGo produced empty output")
	}

	inst, err := NewInstance(wasmBytes)
	if err != nil {
		t.Fatalf("NewInstance failed: %v", err)
	}

	if inst.abi != ABIStdGo {
		t.Fatalf("expected ABIStdGo, got %v", inst.abi)
	}

	addFn := inst.Get("Add")
	if addFn == nil {
		t.Fatal("Add function not found in exported values")
	}

	fn, ok := addFn.(func([]interface{}) interface{})
	if !ok {
		t.Fatalf("Add is not a callable function, got %T", addFn)
	}

	result := fn([]interface{}{float64(2), float64(3)})
	if result == nil {
		t.Fatal("Add returned nil")
	}

	fResult, ok := result.(float64)
	if !ok {
		t.Fatalf("Add returned non-float64: %T %v", result, result)
	}

	if fResult != 5.0 {
		t.Fatalf("Add(2, 3) = %v, want 5", fResult)
	}

	t.Logf("Add(2, 3) = %v", fResult)
}

func TestCompileAndRunTinyGo(t *testing.T) {
	// Check that tinygo is available
	if _, err := exec.LookPath("tinygo"); err != nil {
		t.Skip("tinygo compiler not found in PATH")
	}

	wasmBytes, err := CompileTinyGo("./testdata/tinygo")
	if err != nil {
		t.Fatalf("CompileTinyGo failed: %v", err)
	}

	if len(wasmBytes) == 0 {
		t.Fatal("CompileTinyGo produced empty output")
	}

	inst, err := NewInstance(wasmBytes)
	if err != nil {
		t.Fatalf("NewInstance failed: %v", err)
	}

	if inst.abi != ABITinyGo {
		t.Fatalf("expected ABITinyGo, got %v", inst.abi)
	}

	addFn := inst.Get("Add")
	if addFn == nil {
		t.Fatal("Add function not found in exported values")
	}

	fn, ok := addFn.(func([]interface{}) interface{})
	if !ok {
		t.Fatalf("Add is not a callable function, got %T", addFn)
	}

	result := fn([]interface{}{float64(2), float64(3)})
	if result == nil {
		t.Fatal("Add returned nil")
	}

	fResult, ok := result.(float64)
	if !ok {
		t.Fatalf("Add returned non-float64: %T %v", result, result)
	}

	if fResult != 5.0 {
		t.Fatalf("Add(2, 3) = %v, want 5", fResult)
	}

	t.Logf("Add(2, 3) = %v", fResult)
}

func TestFetchAPI(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go compiler not found in PATH")
	}

	// Start a test HTTP server
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("X-Custom", "test-value")
		fmt.Fprint(w, "hello from host")
	}))
	defer ts.Close()

	wasmBytes, err := CompileGo("./testdata/stdgo_fetch")
	if err != nil {
		t.Fatalf("CompileGo failed: %v", err)
	}

	inst, err := NewInstance(wasmBytes,
		WithFetch(),
		WithArgs([]string{"test", ts.URL}),
	)
	if err != nil {
		t.Fatalf("NewInstance failed: %v", err)
	}

	// Check that the WASM module received the HTTP response
	status := inst.Get("FetchStatus")
	if status == nil {
		t.Fatal("FetchStatus not set — fetch likely failed")
	}
	if s, ok := status.(float64); !ok || s != 200 {
		t.Fatalf("FetchStatus = %v, want 200", status)
	}

	body := inst.Get("FetchBody")
	if body == nil {
		t.Fatal("FetchBody not set")
	}
	if s, ok := body.(string); !ok || s != "hello from host" {
		t.Fatalf("FetchBody = %q, want %q", body, "hello from host")
	}

	t.Logf("Fetch API: status=%v body=%q", status, body)
}
