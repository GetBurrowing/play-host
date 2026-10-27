package main

import (
	"errors"
	"testing"
)

func TestGPUModelIsTheNameOnly(t *testing.T) {
	got, err := gpuModel(func() ([]byte, error) { return []byte("Test GPU 1000\nSecond GPU\n"), nil })
	if err != nil || got != "Test GPU 1000" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := gpuModel(func() ([]byte, error) { return []byte("\n"), nil }); err == nil {
		t.Fatal("empty output must fail")
	}
	if _, err := gpuModel(func() ([]byte, error) { return nil, errors.New("no nvidia-smi") }); err == nil {
		t.Fatal("a missing nvidia-smi must fail")
	}
}
