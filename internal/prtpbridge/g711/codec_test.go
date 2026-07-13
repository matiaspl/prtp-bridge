package g711

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestCustomToneRoundTripPreservesTone(t *testing.T) {
	codec, _, err := New("custom", "")
	if err != nil {
		t.Fatal(err)
	}
	const (
		sampleRate = 8333
		freq       = 1000
		amp        = 6500
		n          = sampleRate
	)
	src := make([]int16, n)
	for i := range src {
		src[i] = int16(math.Round(amp * math.Sin(2*math.Pi*freq*float64(i)/sampleRate)))
	}
	corr, rmsErr := RoundTripCorrelation(codec, src)
	if corr < 0.99 || rmsErr > 200 {
		t.Fatalf("round trip corr=%.6f rmsErr=%.1f, want corr>=0.99 rmsErr<=200", corr, rmsErr)
	}
}

func TestTXMapFormats(t *testing.T) {
	dir := t.TempDir()
	ints := make([]int, 256)
	want := make([]byte, 256)
	for i := range ints {
		ints[i] = 255 - i
		want[i] = byte(255 - i)
	}
	raw, err := json.Marshal(ints)
	if err != nil {
		t.Fatal(err)
	}
	arrayPath := filepath.Join(dir, "array.json")
	if err := os.WriteFile(arrayPath, raw, 0644); err != nil {
		t.Fatal(err)
	}
	objectPath := filepath.Join(dir, "object.json")
	if err := os.WriteFile(objectPath, []byte(`{"type":"kroma-g711-tx-map","tx_map":`+string(raw)+`}`), 0644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{arrayPath, objectPath} {
		got, err := LoadTXMap(path)
		if err != nil {
			t.Fatalf("LoadTXMap(%s): %v", path, err)
		}
		if string(got) != string(want) {
			t.Fatalf("LoadTXMap(%s) mismatch", path)
		}
	}
}

func TestTXMapAppliesAfterEncoding(t *testing.T) {
	plain, _, err := New("custom", "")
	if err != nil {
		t.Fatal(err)
	}
	mapped, _, err := New("custom", "")
	if err != nil {
		t.Fatal(err)
	}
	txMap := make([]byte, 256)
	for i := range txMap {
		txMap[i] = byte(i)
	}
	txMap[plain.Encode([]int16{0})[0]] = 0x7B
	mapped.SetTXMap(txMap)
	got := mapped.Encode([]int16{0})
	if len(got) != 1 || got[0] != 0x7B {
		t.Fatalf("mapped encode = % X, want 7B", got)
	}
	unmapped := mapped.EncodeIntoUnmapped(nil, []int16{0})
	if len(unmapped) != 1 || unmapped[0] == 0x7B || unmapped[0] != plain.Encode([]int16{0})[0] {
		t.Fatalf("unmapped encode = % X, want plain code", unmapped)
	}
}
