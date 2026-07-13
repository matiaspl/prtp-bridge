package webui

import (
	"io/fs"
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
