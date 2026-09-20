//go:build darwin || linux

package term

import (
	"bytes"
	"errors"
	"io"
	"os"
	"testing"
)

func TestInputPumpStopsAndJoins(t *testing.T) {
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = input.Close()
		_ = writer.Close()
	}()
	var output bytes.Buffer
	pump, err := newInputPump(input, &output)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- pump.run() }()
	pump.stop()
	pump.stop()
	if err := <-done; err != nil {
		t.Fatalf("stopped pump returned %v", err)
	}
}

func TestInputPumpReportsEOF(t *testing.T) {
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	defer func() { _ = input.Close() }()
	pump, err := newInputPump(input, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err := pump.run(); !errors.Is(err, io.EOF) {
		t.Fatalf("pump returned %v, want EOF", err)
	}
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return max(0, len(p)-1), nil }

func TestInputPumpReportsPartialWrite(t *testing.T) {
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close() }()
	if _, err := writer.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	pump, err := newInputPump(input, shortWriter{})
	if err != nil {
		t.Fatal(err)
	}
	if err := pump.run(); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("pump returned %v, want short write", err)
	}
}
