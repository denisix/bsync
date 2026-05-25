package main

import (
	"bytes"
	"os"
	"testing"
)

func TestStrictReadvWritevRoundTrip(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "vector-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	parts := [][]byte{[]byte("alpha"), nil, []byte("-"), []byte("omega")}
	want := []byte("alpha-omega")
	n, err := Writev(file, parts)
	if err != nil {
		t.Fatalf("Writev: %v", err)
	}
	if n != len(want) {
		t.Fatalf("Writev bytes = %d, want %d", n, len(want))
	}
	if _, err := file.Seek(0, 0); err != nil {
		t.Fatal(err)
	}

	readParts := [][]byte{make([]byte, 5), {}, make([]byte, 1), make([]byte, 5)}
	n, err = Readv(file, readParts)
	if err != nil {
		t.Fatalf("Readv: %v", err)
	}
	if n != len(want) {
		t.Fatalf("Readv bytes = %d, want %d", n, len(want))
	}
	got := append(append(append([]byte{}, readParts[0]...), readParts[2]...), readParts[3]...)
	if !bytes.Equal(got, want) {
		t.Fatalf("vectored round trip = %q, want %q", got, want)
	}
}

func TestStrictVectorIOEmptyBuffers(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "empty-vector-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	for name, invoke := range map[string]func() (int, error){
		"readv":    func() (int, error) { return Readv(file, [][]byte{nil, {}}) },
		"writev":   func() (int, error) { return Writev(file, nil) },
		"vmsplice": func() (int, error) { return VmSplice(file, [][]byte{{}}) },
	} {
		t.Run(name, func(t *testing.T) {
			n, err := invoke()
			if err != nil || n != 0 {
				t.Fatalf("empty %s = (%d, %v), want (0, nil)", name, n, err)
			}
		})
	}
}

func TestStrictCombinedSplice(t *testing.T) {
	dst, err := os.CreateTemp(t.TempDir(), "splice-*")
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()

	want := []byte("combined-splice-transfer")
	n, err := CombinedSplice(dst, [][]byte{want[:8], {}, want[8:]})
	if err != nil {
		t.Fatalf("CombinedSplice: %v", err)
	}
	if n != int64(len(want)) {
		t.Fatalf("CombinedSplice bytes = %d, want %d", n, len(want))
	}
	got, err := os.ReadFile(dst.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("CombinedSplice output = %q, want %q", got, want)
	}
}
