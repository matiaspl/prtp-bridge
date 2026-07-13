package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"prtp-bridge/internal/prtpbridge/audio"
	"prtp-bridge/internal/prtpbridge/config"
	"prtp-bridge/internal/prtpbridge/matrix"
	"prtp-bridge/internal/prtpbridge/matrixhelper"
	"prtp-bridge/internal/prtpbridge/prtp"

	"github.com/gorilla/websocket"
)

func TestAudioClientNameUsesInstanceByDefault(t *testing.T) {
	cfg := config.Default()
	cfg.Instance = "NET3"
	cfg.MatrixPort = "NET0"
	if got, want := audioClientName(cfg), audio.DefaultClientName+"-NET3"; got != want {
		t.Fatalf("audioClientName() = %q, want %q", got, want)
	}
}

func TestAudioClientNameFallsBackToMatrixPortAndHonorsOverride(t *testing.T) {
	cfg := config.Default()
	cfg.MatrixPort = "NET 1"
	if got, want := audioClientName(cfg), audio.DefaultClientName+"-NET-1"; got != want {
		t.Fatalf("audioClientName() fallback = %q, want %q", got, want)
	}
	cfg.Audio.ClientName = "custom bridge/net"
	if got, want := audioClientName(cfg), "custom-bridge-net"; got != want {
		t.Fatalf("audioClientName() override = %q, want %q", got, want)
	}
}

func TestGatewaySurfacesUnavailableHelperForNames(t *testing.T) {
	cfg := testGatewayConfig(t)
	cfg.MatrixHelperSocket = filepath.Join(filepath.Dir(shortSocketPath(t)), "missing.sock")
	ctx, cancel, errCh := startGateway(t, cfg)
	defer stopGateway(t, cancel, errCh)

	conn := dialControlWS(t, cfg.Listen)
	defer conn.Close()
	if err := conn.WriteJSON(map[string]any{"type": "matrix_fetch_names", "addr": "127.0.0.1:1"}); err != nil {
		t.Fatal(err)
	}
	msg := readWSMessageType(t, conn, "matrix_names")
	if ok, _ := msg["ok"].(bool); ok {
		t.Fatalf("matrix_names unexpectedly succeeded: %+v", msg)
	}
	if msg["error"] == "" {
		t.Fatalf("matrix_names did not include error: %+v", msg)
	}
	_ = ctx
}

func TestGatewaySurfacesHelperCrosspointNotImplemented(t *testing.T) {
	cfg := testGatewayConfig(t)
	ctx, helperCancel := context.WithCancel(context.Background())
	helperErr := make(chan error, 1)
	go func() {
		helperErr <- matrixhelper.NewServer(matrixhelper.Options{
			SocketPath: cfg.MatrixHelperSocket,
			MatrixAddr: "127.0.0.1:1",
			MatrixPort: "NET0",
		}).Serve(ctx)
	}()
	waitHelper(t, cfg.MatrixHelperSocket)
	defer func() {
		helperCancel()
		if err := <-helperErr; err != nil {
			t.Fatal(err)
		}
	}()

	_, cancel, errCh := startGateway(t, cfg)
	defer stopGateway(t, cancel, errCh)

	conn := dialControlWS(t, cfg.Listen)
	defer conn.Close()
	if err := conn.WriteJSON(map[string]any{"type": "matrix_crosspoint", "addr": "127.0.0.1:1", "xin": 3, "xout": 7, "enabled": true}); err != nil {
		t.Fatal(err)
	}
	msg := readWSMessageType(t, conn, "matrix_crosspoint")
	if ok, _ := msg["ok"].(bool); ok {
		t.Fatalf("matrix_crosspoint unexpectedly succeeded: %+v", msg)
	}
	if code, _ := msg["code"].(string); code != "not_implemented" {
		t.Fatalf("matrix_crosspoint code = %q, msg=%+v", code, msg)
	}
}

func TestEmptyIdentityRequestDoesNotCacheBlankIdent(t *testing.T) {
	h := newHub()
	emitInfo(h, []byte{0x49, 0xD0}, nil, nil, nil, nil, false)
	if h.hasSnapshotKey("prtp_info:ident") {
		t.Fatal("empty identity request was cached as an identity value")
	}
}

func TestRFrameIsAcknowledged(t *testing.T) {
	h := newHub()
	var sent [][]byte
	emitInfo(h, []byte{0x52, 0x01}, func(frame []byte) {
		sent = append(sent, append([]byte(nil), frame...))
	}, nil, nil, nil, false)
	if len(sent) != 1 {
		t.Fatalf("sent %d frames, want 1 ACK", len(sent))
	}
	if want := []byte{0x41, 0x4B, 0xFF}; !bytes.Equal(sent[0], want) {
		t.Fatalf("ACK frame = % X, want % X", sent[0], want)
	}
}

func TestBP7100PingIsAcknowledgedWithoutSynchronizingState(t *testing.T) {
	h := newHub()
	emu, err := prtp.NewEmulationProfile("bp7100", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	var sent [][]byte
	sentSync := false
	emitInfo(h, []byte{0x50}, func(frame []byte) {
		sent = append(sent, append([]byte(nil), frame...))
	}, nil, &sentSync, emu, false)
	emitInfo(h, []byte{0x50}, func(frame []byte) {
		sent = append(sent, append([]byte(nil), frame...))
	}, nil, &sentSync, emu, false)
	if len(sent) != 2 {
		t.Fatalf("sent %d frames, want one ACK per ping and no sync", len(sent))
	}
	if want := []byte{0x41, 0x4B, 0xFF}; !bytes.Equal(sent[0], want) {
		t.Fatalf("first ACK frame = % X, want % X", sent[0], want)
	} else if !bytes.Equal(sent[1], want) {
		t.Fatalf("second ACK frame = % X, want % X", sent[1], want)
	}
	if sentSync {
		t.Fatal("BP7100 state was marked synchronized")
	}
}

func TestPanelPingSynchronizesStateOnce(t *testing.T) {
	h := newHub()
	emu, err := prtp.NewEmulationProfile("tp5024", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	var sent [][]byte
	sentSync := false
	send := func(frame []byte) {
		sent = append(sent, append([]byte(nil), frame...))
	}
	emitInfo(h, []byte{0x50}, send, nil, &sentSync, emu, false)
	emitInfo(h, []byte{0x50}, send, nil, &sentSync, emu, false)
	if len(sent) != 3 {
		t.Fatalf("sent %d frames, want ACK + sync + ACK", len(sent))
	}
	if want := []byte{0x53, 0xC5, 0xFF}; !bytes.Equal(sent[1], want) {
		t.Fatalf("sync frame = % X, want % X", sent[1], want)
	}
	if !sentSync {
		t.Fatal("panel state was not marked synchronized")
	}
}

func TestBP7100InvalidFrameRequestsRetransmit(t *testing.T) {
	emu, err := prtp.NewEmulationProfile("bp7100", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := invalidPRTPFrameResponse(emu), []byte{0x4E, 0xDD, 0xFF}; !bytes.Equal(got, want) {
		t.Fatalf("invalid-frame response = % X, want NACK % X", got, want)
	}
}

func TestPanelInvalidFrameDoesNotChangeExistingBehavior(t *testing.T) {
	emu, err := prtp.NewEmulationProfile("tp5024", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := invalidPRTPFrameResponse(emu); got != nil {
		t.Fatalf("panel invalid-frame response = % X, want none", got)
	}
}

func TestRXFragmentAssemblerRecoversDigiSplitPacket(t *testing.T) {
	packet := testAudioPacket(0x42)
	addr := &net.UDPAddr{IP: net.ParseIP("192.168.7.203"), Port: 8087}
	var fragments rxFragmentAssembler

	ready, stats := fragments.push(time.Unix(0, 0), addr, packet[:267])
	if len(ready) != 0 {
		t.Fatalf("first fragment produced %d packets, want 0", len(ready))
	}
	if stats.recovered != 0 || stats.dropped != 0 {
		t.Fatalf("first fragment stats = %+v, want zero", stats)
	}

	ready, stats = fragments.push(time.Unix(0, int64(time.Millisecond)), addr, packet[267:])
	if stats.recovered != 1 || stats.dropped != 0 {
		t.Fatalf("second fragment stats = %+v, want recovered=1 dropped=0", stats)
	}
	if len(ready) != 1 {
		t.Fatalf("second fragment produced %d packets, want 1", len(ready))
	}
	if !bytes.Equal(ready[0].packet, packet) {
		t.Fatalf("assembled packet differs from original")
	}
}

func TestRXFragmentAssemblerDropsStaleFragment(t *testing.T) {
	packet := testAudioPacket(0x42)
	addr := &net.UDPAddr{IP: net.ParseIP("192.168.7.203"), Port: 8087}
	var fragments rxFragmentAssembler

	ready, stats := fragments.push(time.Unix(0, 0), addr, packet[:267])
	if len(ready) != 0 || stats.dropped != 0 {
		t.Fatalf("first fragment ready=%d stats=%+v, want no output", len(ready), stats)
	}
	ready, stats = fragments.push(time.Unix(0, int64(101*time.Millisecond)), addr, packet[267:])
	if len(ready) != 0 {
		t.Fatalf("stale completion produced %d packets, want 0", len(ready))
	}
	if stats.dropped != 1 {
		t.Fatalf("stale completion stats = %+v, want dropped=1", stats)
	}
}

func TestPRTPControlQueuePrioritizesWholeFrames(t *testing.T) {
	var q prtpControlQueue
	q.enqueue([]byte{0x10, 0x11}, prtpControlNormal)
	q.enqueue([]byte{0x20, 0x21}, prtpControlHigh)
	if got := q.pop(4); string(got) != string([]byte{0x20, 0x21}) {
		t.Fatalf("first pop = % x, want high-priority frame", got)
	}
	if got := q.pop(4); string(got) != string([]byte{0x10, 0x11}) {
		t.Fatalf("second pop = % x, want normal frame", got)
	}
}

func TestPRTPControlQueueDoesNotInterleaveActiveFrame(t *testing.T) {
	var q prtpControlQueue
	q.enqueue([]byte{0x10, 0x11, 0x12, 0x13, 0x14}, prtpControlNormal)
	if got := q.pop(3); string(got) != string([]byte{0x10, 0x11, 0x12}) {
		t.Fatalf("first pop = % x", got)
	}
	q.enqueue([]byte{0x20, 0x21}, prtpControlHigh)
	if got := q.pop(4); string(got) != string([]byte{0x13, 0x14}) {
		t.Fatalf("second pop = % x, active normal frame was interleaved", got)
	}
	if got := q.pop(4); string(got) != string([]byte{0x20, 0x21}) {
		t.Fatalf("third pop = % x, want high-priority frame", got)
	}
}

func TestRXReorderBufferReordersLatePacket(t *testing.T) {
	b := newRXReorderBuffer(20 * time.Millisecond)
	now := time.Unix(0, 0)
	if ready, stats := b.push(now, nil, testAudioPacket(1)); len(ready) != 1 || stats != (rxReorderStats{}) {
		t.Fatalf("seq1 ready=%d stats=%+v", len(ready), stats)
	}
	if ready, stats := b.push(now.Add(time.Millisecond), nil, testAudioPacket(3)); len(ready) != 0 || stats.gapEvents != 1 {
		t.Fatalf("seq3 ready=%d stats=%+v", len(ready), stats)
	}
	ready, stats := b.push(now.Add(2*time.Millisecond), nil, testAudioPacket(2))
	if len(ready) != 2 || ready[0].packet[1] != 2 || ready[1].packet[1] != 3 {
		t.Fatalf("ready order = %v", packetSeqs(ready))
	}
	if stats.reordered != 1 || stats.missing != 0 || stats.stale != 0 {
		t.Fatalf("stats=%+v, want reordered=1 only", stats)
	}
}

func TestRXReorderBufferSkipsMissingAfterWait(t *testing.T) {
	b := newRXReorderBuffer(20 * time.Millisecond)
	now := time.Unix(0, 0)
	if ready, _ := b.push(now, nil, testAudioPacket(1)); len(ready) != 1 {
		t.Fatalf("seq1 ready=%d", len(ready))
	}
	if ready, stats := b.push(now.Add(time.Millisecond), nil, testAudioPacket(3)); len(ready) != 0 || stats.gapEvents != 1 {
		t.Fatalf("seq3 ready=%d stats=%+v", len(ready), stats)
	}
	var stats rxReorderStats
	ready := b.flushExpired(now.Add(25*time.Millisecond), &stats)
	if len(ready) != 1 || ready[0].packet[1] != 3 {
		t.Fatalf("ready order = %v", packetSeqs(ready))
	}
	if stats.missing != 1 || stats.reordered != 0 {
		t.Fatalf("stats=%+v, want missing=1 reordered=0", stats)
	}
}

func TestRXSequenceTrackerCountsGapsWithoutReordering(t *testing.T) {
	var tracker rxSequenceTracker
	if stats := tracker.observe(testAudioPacket(10)); stats != (rxReorderStats{}) {
		t.Fatalf("first packet stats=%+v", stats)
	}
	stats := tracker.observe(testAudioPacket(12))
	if stats.gapEvents != 1 || stats.missing != 1 || stats.reordered != 0 {
		t.Fatalf("gap stats=%+v, want one missing packet and no reorder", stats)
	}
	if stats := tracker.observe(testAudioPacket(11)); stats.stale != 1 {
		t.Fatalf("late packet stats=%+v, want stale=1", stats)
	}
}

func TestEmulationProfileFromMatrixTarget(t *testing.T) {
	cfg := config.Default().Emulation
	cfg.Device = "auto"
	tests := []struct {
		name      string
		target    matrix.PortName
		wantModel string
		wantName  string
		wantOK    bool
	}{
		{
			name:      "net3 tp5024",
			target:    matrix.PortName{Port: "NET3", TypeCode: int(prtp.TypeTP5024), TypeName: "TP5024"},
			wantModel: "TP5024",
			wantName:  "TP5024-NET3",
			wantOK:    true,
		},
		{
			name:      "net0 bp7100",
			target:    matrix.PortName{Port: "NET0", TypeCode: int(prtp.TypeBP7100), TypeName: "BP7100"},
			wantModel: "BP7100",
			wantName:  "BP7100-NET0",
			wantOK:    true,
		},
		{
			name:   "unsupported",
			target: matrix.PortName{Port: "AUDIO", TypeCode: 0x1A, TypeName: "AUDIO"},
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok, err := emulationProfileFromMatrixTarget(tt.target, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if got.Model != tt.wantModel || got.Name != tt.wantName {
				t.Fatalf("profile = %+v, want model=%s name=%s", got, tt.wantModel, tt.wantName)
			}
		})
	}
}

func testAudioPacket(seq byte) []byte {
	return prtp.BuildAudioPacket(seq, make([]byte, prtp.AudioPayloadSize), nil)
}

func packetSeqs(packets []rxBufferedPacket) []byte {
	seqs := make([]byte, 0, len(packets))
	for _, packet := range packets {
		if len(packet.packet) > 1 {
			seqs = append(seqs, packet.packet[1])
		}
	}
	return seqs
}

func testGatewayConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Listen = freeTCPAddr(t)
	cfg.UDP.Bind = "127.0.0.1:0"
	cfg.MatrixHelperSocket = shortSocketPath(t)
	cfg.MatrixAddr = "127.0.0.1:1"
	cfg.MatrixPort = "NET0"
	return cfg
}

func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "kroma-gateway-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "h.sock")
}

func startGateway(t *testing.T, cfg config.Config) (context.Context, context.CancelFunc, chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- Serve(ctx, cfg)
	}()
	waitHTTP(t, "http://"+cfg.Listen+"/")
	return ctx, cancel, errCh
}

func stopGateway(t *testing.T, cancel context.CancelFunc, errCh chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("gateway did not stop")
	}
}

func freeTCPAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func waitHTTP(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("HTTP server did not become ready: %v", lastErr)
}

func waitHelper(t *testing.T, socket string) {
	t.Helper()
	client := matrixhelper.NewClient(socket)
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		lastErr = client.Health(ctx)
		cancel()
		if lastErr == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("helper did not become ready: %v", lastErr)
}

func dialControlWS(t *testing.T, addr string) *websocket.Conn {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, _, err := websocket.DefaultDialer.Dial("ws://"+addr+"/control", nil)
		if err == nil {
			return conn
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("control websocket did not become ready: %v", lastErr)
	return nil
}

func readWSMessageType(t *testing.T, conn *websocket.Conn, want string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		_, data, err := conn.ReadMessage()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			t.Fatal(err)
		}
		var msg map[string]any
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		if typ, _ := msg["type"].(string); typ == want {
			return msg
		}
	}
	t.Fatalf("did not receive websocket message type %q", want)
	return nil
}
