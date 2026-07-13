package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadResolveInstance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.json")
	if err := os.WriteFile(path, []byte(`{
  "listen": "0.0.0.0:8090",
  "udp": {"bind": ":8087", "rate": 8333},
  "websocket_paths": {"control": "/control", "audio": "/audio-stream"},
  "audio": {"source": "ws", "local_rate": 16000},
  "matrix_helper_socket": "/tmp/kroma-helper.sock",
  "matrix_addr": "192.168.7.113",
  "matrix_port": "NET3",
  "g711": {"mode": "custom"},
  "emulation": {"device": "auto"},
	  "instances": {
	    "NET0": {
	      "listen": "0.0.0.0:8090",
	      "udp": {"bind": ":8087"},
	      "matrix_port": "NET0",
	      "audio": {"client_name": "custom-net0"}
	    },
    "NET3": {
      "listen": "0.0.0.0:8093",
      "udp": {"bind": ":8090"},
      "matrix_port": "NET3"
    }
  }
}`), 0644); err != nil {
		t.Fatal(err)
	}
	root, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	net0, err := root.ResolveInstance("NET0")
	if err != nil {
		t.Fatal(err)
	}
	if net0.Instance != "NET0" || net0.MatrixPort != "NET0" || net0.UDP.Rate != 8333 || net0.WebSocketPaths.Audio != "/audio-stream" {
		t.Fatalf("NET0 = %+v", net0)
	}
	if net0.Audio.ClientName != "custom-net0" {
		t.Fatalf("NET0 audio client name = %q", net0.Audio.ClientName)
	}
	if net0.Emulation.Device != "auto" {
		t.Fatalf("NET0 emulation device = %q", net0.Emulation.Device)
	}
	net3, err := root.ResolveInstance("NET3")
	if err != nil {
		t.Fatal(err)
	}
	if net3.Listen != "0.0.0.0:8093" || net3.UDP.Bind != ":8090" {
		t.Fatalf("NET3 = %+v", net3)
	}
}

func TestLoadAcceptsLegacySimulationAlias(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bridge.json")
	if err := os.WriteFile(path, []byte(`{
  "listen": "0.0.0.0:8090",
  "udp": {"bind": ":8087", "rate": 8333},
  "websocket_paths": {"control": "/control", "audio": "/audio-stream"},
  "audio": {"source": "ws", "local_rate": 16000},
  "matrix_helper_socket": "/tmp/kroma-helper.sock",
  "matrix_port": "NET3",
  "g711": {"mode": "custom"},
  "simulation": {"device": "tp5024", "name": "legacy-name"},
  "instances": {
    "NET3": {
      "listen": "0.0.0.0:8093",
      "simulation": {"name": "legacy-net3"}
    }
  }
}`), 0644); err != nil {
		t.Fatal(err)
	}
	root, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	net3, err := root.ResolveInstance("NET3")
	if err != nil {
		t.Fatal(err)
	}
	if net3.Emulation.Device != "tp5024" || net3.Emulation.Name != "legacy-net3" {
		t.Fatalf("legacy simulation alias was not applied: %+v", net3.Emulation)
	}
}

func TestValidateRejectsDuplicateWebSocketPaths(t *testing.T) {
	cfg := Default()
	cfg.WebSocketPaths.Audio = cfg.WebSocketPaths.Control
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate accepted duplicate websocket paths")
	}
}

func TestValidateRejectsNegativeRXReorder(t *testing.T) {
	cfg := Default()
	cfg.UDP.RXReorderMS = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate accepted negative udp.rx_reorder_ms")
	}
}
