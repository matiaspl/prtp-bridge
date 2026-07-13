package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"prtp-bridge/internal/prtpbridge/matrix"
)

type Config struct {
	Instance           string                    `json:"-"`
	Listen             string                    `json:"listen"`
	UDP                UDPConfig                 `json:"udp"`
	WebSocketPaths     WebSocketPaths            `json:"websocket_paths"`
	Audio              AudioConfig               `json:"audio"`
	MatrixHelperSocket string                    `json:"matrix_helper_socket"`
	MatrixAddr         string                    `json:"matrix_addr"`
	MatrixPort         string                    `json:"matrix_port"`
	G711               G711Config                `json:"g711"`
	TLS                TLSConfig                 `json:"tls"`
	Emulation          EmulationConfig           `json:"emulation"`
	Simulation         EmulationConfig           `json:"simulation,omitempty"` // Deprecated legacy alias for emulation.
	Dump               DumpConfig                `json:"dump"`
	Debug              DebugConfig               `json:"debug"`
	Instances          map[string]InstanceConfig `json:"instances,omitempty"`
}

type InstanceConfig struct {
	Listen             string          `json:"listen,omitempty"`
	UDP                UDPConfig       `json:"udp,omitempty"`
	WebSocketPaths     WebSocketPaths  `json:"websocket_paths,omitempty"`
	Audio              AudioConfig     `json:"audio,omitempty"`
	MatrixHelperSocket string          `json:"matrix_helper_socket,omitempty"`
	MatrixAddr         string          `json:"matrix_addr,omitempty"`
	MatrixPort         string          `json:"matrix_port,omitempty"`
	G711               G711Config      `json:"g711,omitempty"`
	TLS                TLSConfig       `json:"tls,omitempty"`
	Emulation          EmulationConfig `json:"emulation,omitempty"`
	Simulation         EmulationConfig `json:"simulation,omitempty"` // Deprecated legacy alias for emulation.
	Dump               DumpConfig      `json:"dump,omitempty"`
	Debug              DebugConfig     `json:"debug,omitempty"`
}

type UDPConfig struct {
	Bind        string `json:"bind,omitempty"`
	Peer        string `json:"peer,omitempty"`
	Rate        int    `json:"rate,omitempty"`
	RXReorderMS int    `json:"rx_reorder_ms,omitempty"`
}

type WebSocketPaths struct {
	Control string `json:"control,omitempty"`
	Audio   string `json:"audio,omitempty"`
}

type AudioConfig struct {
	LocalPlayback bool    `json:"local_playback,omitempty"`
	LocalCapture  bool    `json:"local_capture,omitempty"`
	LocalRate     int     `json:"local_rate,omitempty"`
	Source        string  `json:"source,omitempty"`
	ToneFreq      float64 `json:"tone_freq,omitempty"`
	FIRFilter     bool    `json:"fir_filter,omitempty"`
	CaptureID     string  `json:"capture_id,omitempty"`
	PlaybackID    string  `json:"playback_id,omitempty"`
	ClientName    string  `json:"client_name,omitempty"`
}

type G711Config struct {
	Mode  string `json:"mode,omitempty"`
	Table string `json:"table,omitempty"`
	TXMap string `json:"tx_map,omitempty"`
}

type TLSConfig struct {
	Cert string `json:"cert,omitempty"`
	Key  string `json:"key,omitempty"`
}

type EmulationConfig struct {
	Device      string `json:"device,omitempty"`
	Name        string `json:"name,omitempty"`
	Keys        int    `json:"keys,omitempty"`
	UserVersion string `json:"user_version,omitempty"`
	FPGAVersion string `json:"fpga_version,omitempty"`
}

type DumpConfig struct {
	MatrixDir    string `json:"matrix_dir,omitempty"`
	MatrixRxPCM  string `json:"matrix_rx_pcm,omitempty"`
	MatrixTxPCM  string `json:"matrix_tx_pcm,omitempty"`
	MatrixRxG711 string `json:"matrix_rx_g711,omitempty"`
	MatrixTxG711 string `json:"matrix_tx_g711,omitempty"`
}

type DebugConfig struct {
	PRTP   bool `json:"prtp,omitempty"`
	Matrix bool `json:"matrix,omitempty"`
}

func Default() Config {
	return Config{
		Listen: "0.0.0.0:8090",
		UDP: UDPConfig{
			Bind: ":8087",
			Rate: 8333,
		},
		WebSocketPaths: WebSocketPaths{
			Control: "/control",
			Audio:   "/audio-stream",
		},
		Audio: AudioConfig{
			LocalRate:  16000,
			Source:     "ws",
			ToneFreq:   1000,
			CaptureID:  "default",
			PlaybackID: "default",
		},
		MatrixHelperSocket: "/tmp/prtp-matrix-helper.sock",
		MatrixPort:         "NET3",
		G711: G711Config{
			Mode: "custom",
		},
		Emulation: EmulationConfig{
			UserVersion: "1.03.000",
			FPGAVersion: "0.0",
		},
	}
}

func Load(path string) (Config, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return Config{}, errors.New("config path is required")
	}
	cfg := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse JSON config: %w", err)
	}
	applyLegacyAliases(&cfg)
	applyDefaults(&cfg)
	return cfg, nil
}

func (c Config) ResolveInstance(instance string) (Config, error) {
	base := c
	base.Instances = nil
	instance = strings.TrimSpace(instance)
	if instance == "" {
		applyLegacyAliases(&base)
		applyDefaults(&base)
		return base, base.Validate()
	}
	inst, ok := c.Instances[instance]
	if !ok {
		return Config{}, fmt.Errorf("instance %q not found", instance)
	}
	mergeEmulationAlias(&inst.Emulation, inst.Simulation)
	mergeInstance(&base, inst)
	base.Instance = instance
	applyLegacyAliases(&base)
	applyDefaults(&base)
	return base, base.Validate()
}

func (c Config) Validate() error {
	if strings.TrimSpace(c.Listen) == "" {
		return errors.New("listen must not be empty")
	}
	if strings.TrimSpace(c.UDP.Bind) == "" {
		return errors.New("udp.bind must not be empty")
	}
	if c.UDP.Rate <= 0 {
		return errors.New("udp.rate must be positive")
	}
	if c.UDP.Rate != 8000 && c.UDP.Rate != 8333 {
		return fmt.Errorf("udp.rate must be 8000 or 8333, got %d", c.UDP.Rate)
	}
	if c.UDP.RXReorderMS < 0 {
		return fmt.Errorf("udp.rx_reorder_ms must be non-negative, got %d", c.UDP.RXReorderMS)
	}
	control, err := NormalizeWSPath(c.WebSocketPaths.Control, "websocket_paths.control")
	if err != nil {
		return err
	}
	audio, err := NormalizeWSPath(c.WebSocketPaths.Audio, "websocket_paths.audio")
	if err != nil {
		return err
	}
	if control == audio {
		return fmt.Errorf("websocket paths must be unique: %s", control)
	}
	if _, ok := NormalizeAudioSource(c.Audio.Source); !ok {
		return fmt.Errorf("invalid audio.source %q (expected ws|server|silence|tone|echo)", c.Audio.Source)
	}
	if c.Audio.LocalRate <= 0 {
		return errors.New("audio.local_rate must be positive")
	}
	mode := strings.ToLower(strings.TrimSpace(c.G711.Mode))
	if mode != "custom" && mode != "alaw" {
		return fmt.Errorf("g711.mode must be custom or alaw, got %q", c.G711.Mode)
	}
	if (strings.TrimSpace(c.TLS.Cert) == "") != (strings.TrimSpace(c.TLS.Key) == "") {
		return errors.New("tls.cert and tls.key must be provided together")
	}
	if _, err := matrix.ParsePortRef(c.MatrixPort); err != nil {
		return err
	}
	if strings.TrimSpace(c.MatrixHelperSocket) == "" {
		return errors.New("matrix_helper_socket must not be empty")
	}
	return nil
}

func NormalizeWSPath(raw, name string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("%s must not be empty", name)
	}
	if !strings.HasPrefix(raw, "/") {
		raw = "/" + raw
	}
	return raw, nil
}

func NormalizeAudioSource(source string) (string, bool) {
	source = strings.ToLower(strings.TrimSpace(source))
	if source == "" {
		source = "ws"
	}
	switch source {
	case "ws", "server", "silence", "tone", "echo":
		return source, true
	default:
		return source, false
	}
}

func applyDefaults(c *Config) {
	d := Default()
	if c.Listen == "" {
		c.Listen = d.Listen
	}
	if c.UDP.Bind == "" {
		c.UDP.Bind = d.UDP.Bind
	}
	if c.UDP.Rate == 0 {
		c.UDP.Rate = d.UDP.Rate
	}
	if c.WebSocketPaths.Control == "" {
		c.WebSocketPaths.Control = d.WebSocketPaths.Control
	}
	if c.WebSocketPaths.Audio == "" {
		c.WebSocketPaths.Audio = d.WebSocketPaths.Audio
	}
	if c.Audio.LocalRate == 0 {
		c.Audio.LocalRate = d.Audio.LocalRate
	}
	if c.Audio.Source == "" {
		c.Audio.Source = d.Audio.Source
	}
	if c.Audio.ToneFreq == 0 {
		c.Audio.ToneFreq = d.Audio.ToneFreq
	}
	if c.Audio.CaptureID == "" {
		c.Audio.CaptureID = d.Audio.CaptureID
	}
	if c.Audio.PlaybackID == "" {
		c.Audio.PlaybackID = d.Audio.PlaybackID
	}
	if c.MatrixHelperSocket == "" {
		c.MatrixHelperSocket = d.MatrixHelperSocket
	}
	if c.MatrixPort == "" {
		c.MatrixPort = d.MatrixPort
	}
	if c.G711.Mode == "" {
		c.G711.Mode = d.G711.Mode
	}
	if c.Emulation.UserVersion == "" {
		c.Emulation.UserVersion = d.Emulation.UserVersion
	}
	if c.Emulation.FPGAVersion == "" {
		c.Emulation.FPGAVersion = d.Emulation.FPGAVersion
	}
}

func applyLegacyAliases(c *Config) {
	mergeEmulationAlias(&c.Emulation, c.Simulation)
	for name, inst := range c.Instances {
		mergeEmulationAlias(&inst.Emulation, inst.Simulation)
		c.Instances[name] = inst
	}
}

func mergeEmulationAlias(dst *EmulationConfig, legacy EmulationConfig) {
	if dst.Device == "" {
		dst.Device = legacy.Device
	}
	if dst.Name == "" {
		dst.Name = legacy.Name
	}
	if dst.Keys == 0 {
		dst.Keys = legacy.Keys
	}
	if dst.UserVersion == "" {
		dst.UserVersion = legacy.UserVersion
	}
	if dst.FPGAVersion == "" {
		dst.FPGAVersion = legacy.FPGAVersion
	}
}

func mergeInstance(dst *Config, inst InstanceConfig) {
	if inst.Listen != "" {
		dst.Listen = inst.Listen
	}
	if inst.UDP.Bind != "" {
		dst.UDP.Bind = inst.UDP.Bind
	}
	if inst.UDP.Peer != "" {
		dst.UDP.Peer = inst.UDP.Peer
	}
	if inst.UDP.Rate != 0 {
		dst.UDP.Rate = inst.UDP.Rate
	}
	if inst.UDP.RXReorderMS != 0 {
		dst.UDP.RXReorderMS = inst.UDP.RXReorderMS
	}
	if inst.WebSocketPaths.Control != "" {
		dst.WebSocketPaths.Control = inst.WebSocketPaths.Control
	}
	if inst.WebSocketPaths.Audio != "" {
		dst.WebSocketPaths.Audio = inst.WebSocketPaths.Audio
	}
	if inst.Audio.LocalPlayback {
		dst.Audio.LocalPlayback = inst.Audio.LocalPlayback
	}
	if inst.Audio.LocalCapture {
		dst.Audio.LocalCapture = inst.Audio.LocalCapture
	}
	if inst.Audio.LocalRate != 0 {
		dst.Audio.LocalRate = inst.Audio.LocalRate
	}
	if inst.Audio.Source != "" {
		dst.Audio.Source = inst.Audio.Source
	}
	if inst.Audio.ToneFreq != 0 {
		dst.Audio.ToneFreq = inst.Audio.ToneFreq
	}
	if inst.Audio.FIRFilter {
		dst.Audio.FIRFilter = inst.Audio.FIRFilter
	}
	if inst.Audio.CaptureID != "" {
		dst.Audio.CaptureID = inst.Audio.CaptureID
	}
	if inst.Audio.PlaybackID != "" {
		dst.Audio.PlaybackID = inst.Audio.PlaybackID
	}
	if inst.Audio.ClientName != "" {
		dst.Audio.ClientName = inst.Audio.ClientName
	}
	if inst.MatrixHelperSocket != "" {
		dst.MatrixHelperSocket = inst.MatrixHelperSocket
	}
	if inst.MatrixAddr != "" {
		dst.MatrixAddr = inst.MatrixAddr
	}
	if inst.MatrixPort != "" {
		dst.MatrixPort = inst.MatrixPort
	}
	if inst.G711.Mode != "" {
		dst.G711.Mode = inst.G711.Mode
	}
	if inst.G711.Table != "" {
		dst.G711.Table = inst.G711.Table
	}
	if inst.G711.TXMap != "" {
		dst.G711.TXMap = inst.G711.TXMap
	}
	if inst.TLS.Cert != "" {
		dst.TLS.Cert = inst.TLS.Cert
	}
	if inst.TLS.Key != "" {
		dst.TLS.Key = inst.TLS.Key
	}
	if inst.Emulation.Device != "" {
		dst.Emulation.Device = inst.Emulation.Device
	}
	if inst.Emulation.Name != "" {
		dst.Emulation.Name = inst.Emulation.Name
	}
	if inst.Emulation.Keys != 0 {
		dst.Emulation.Keys = inst.Emulation.Keys
	}
	if inst.Emulation.UserVersion != "" {
		dst.Emulation.UserVersion = inst.Emulation.UserVersion
	}
	if inst.Emulation.FPGAVersion != "" {
		dst.Emulation.FPGAVersion = inst.Emulation.FPGAVersion
	}
	if inst.Dump.MatrixDir != "" {
		dst.Dump.MatrixDir = inst.Dump.MatrixDir
	}
	if inst.Dump.MatrixRxPCM != "" {
		dst.Dump.MatrixRxPCM = inst.Dump.MatrixRxPCM
	}
	if inst.Dump.MatrixTxPCM != "" {
		dst.Dump.MatrixTxPCM = inst.Dump.MatrixTxPCM
	}
	if inst.Dump.MatrixRxG711 != "" {
		dst.Dump.MatrixRxG711 = inst.Dump.MatrixRxG711
	}
	if inst.Dump.MatrixTxG711 != "" {
		dst.Dump.MatrixTxG711 = inst.Dump.MatrixTxG711
	}
	if inst.Debug.PRTP {
		dst.Debug.PRTP = inst.Debug.PRTP
	}
	if inst.Debug.Matrix {
		dst.Debug.Matrix = inst.Debug.Matrix
	}
}
