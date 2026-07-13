package audio

import (
	"math"
	"path/filepath"
	"strings"
	"testing"
)

func TestStreamResamplerKeepsLongTermRateAcrossChunks(t *testing.T) {
	r := NewInt16StreamResampler(16000, 8333)
	total := 0
	for chunk := 0; chunk < 100; chunk++ {
		in := make([]int16, 320)
		for i := range in {
			in[i] = int16((chunk*len(in) + i) % 2000)
		}
		total += len(r.Process(in))
	}
	want := int(math.Round(float64(100*320) * 8333 / 16000))
	if diff := total - want; diff < -1 || diff > 1 {
		t.Fatalf("stream resampler output samples = %d, want about %d", total, want)
	}
}

func TestTXAntiAliasFIROnlyWhenDownsampling(t *testing.T) {
	tests := []struct {
		name    string
		enabled bool
		inRate  int
		outRate int
		want    bool
	}{
		{name: "disabled", enabled: false, inRate: 16000, outRate: 8333, want: false},
		{name: "downsample", enabled: true, inRate: 16000, outRate: 8333, want: true},
		{name: "same rate", enabled: true, inRate: 8333, outRate: 8333, want: false},
		{name: "upsample", enabled: true, inRate: 8000, outRate: 8333, want: false},
		{name: "invalid input", enabled: true, inRate: 0, outRate: 8333, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NewTXAntiAliasFIR(tt.enabled, tt.inRate, tt.outRate) != nil
			if got != tt.want {
				t.Fatalf("NewTXAntiAliasFIR active = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNormalizeClientName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "default", in: "", want: DefaultClientName},
		{name: "trim and replace spaces", in: "  prtp bridge NET3  ", want: "prtp-bridge-NET3"},
		{name: "drop separators only", in: ":::", want: DefaultClientName},
		{name: "preserve safe chars", in: "prtp_bridge-NET0", want: "prtp_bridge-NET0"},
		{name: "replace unsafe runs", in: "PRTP:NET 1", want: "PRTP-NET-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeClientName(tt.in); got != tt.want {
				t.Fatalf("NormalizeClientName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestNormalizeClientNameTruncatesToJackSafeLimit(t *testing.T) {
	got := NormalizeClientName(strings.Repeat("a", maxClientNameLen+20))
	if len(got) != maxClientNameLen {
		t.Fatalf("normalized client name length = %d, want %d", len(got), maxClientNameLen)
	}
}

func TestResolveDumpPath(t *testing.T) {
	dir := t.TempDir()
	got, isPipe, err := ResolveDumpPath(dir, "matrix-rx", "s16le")
	if err != nil {
		t.Fatal(err)
	}
	if isPipe || filepath.Dir(got) != dir || !strings.HasSuffix(filepath.Base(got), "-matrix-rx.s16le") {
		t.Fatalf("directory dump path = %q isPipe=%v", got, isPipe)
	}
	got, isPipe, err = ResolveDumpPath(filepath.Join(dir, "{ts}-{name}.{ext}"), "matrix-tx", "g711")
	if err != nil {
		t.Fatal(err)
	}
	if isPipe || !strings.HasSuffix(got, "-matrix-tx.g711") {
		t.Fatalf("templated dump path = %q isPipe=%v", got, isPipe)
	}
}
