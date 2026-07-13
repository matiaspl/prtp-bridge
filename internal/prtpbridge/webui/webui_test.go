package webui

import (
	"io/fs"
	"strings"
	"testing"
)

func TestEmbeddedAudioWorklet(t *testing.T) {
	data, err := fs.ReadFile(FS(), "rx-worklet.js")
	if err != nil {
		t.Fatalf("read embedded RX worklet: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("embedded RX worklet is empty")
	}
}

func TestEmbeddedAppKeepsBP7100Keyless(t *testing.T) {
	data, err := fs.ReadFile(FS(), "app.js")
	if err != nil {
		t.Fatalf("read embedded app: %v", err)
	}
	source := string(data)
	for _, want := range []string{
		"BP7100: 0",
		"return keys === 0 ? 'no keys'",
		"n = Math.max(0, n | 0)",
	} {
		if !strings.Contains(source, want) {
			t.Fatalf("embedded app is missing keyless BP7100 behavior %q", want)
		}
	}
}
