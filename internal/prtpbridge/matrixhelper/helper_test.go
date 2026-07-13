package matrixhelper

import (
	"bufio"
	"context"
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"prtp-bridge/internal/prtpbridge/matrix"
)

func TestHelperHealthAndCrosspointNotImplemented(t *testing.T) {
	socket := shortSocketPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- NewServer(Options{SocketPath: socket, MatrixAddr: "127.0.0.1:1", MatrixPort: "NET0"}).Serve(ctx)
	}()
	client := NewClient(socket)
	waitHelperHealth(t, client)

	resp, err := client.Crosspoint(context.Background(), CrosspointRequest{XIn: 3, XOut: 7, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if resp.OK || resp.Code != "not_implemented" {
		t.Fatalf("crosspoint response = %+v", resp)
	}
	cancel()
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
}

func TestHelperNamesWithFakeMatrixServer(t *testing.T) {
	addr := startFakeMatrixServer(t)
	socket := shortSocketPath(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- NewServer(Options{
			SocketPath:      socket,
			MatrixAddr:      addr,
			MatrixPort:      "NET3",
			ConnectTimeout:  time.Second,
			ExchangeTimeout: time.Second,
		}).Serve(ctx)
	}()
	client := NewClient(socket)
	waitHelperHealth(t, client)

	snap, err := client.Names(context.Background(), NamesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Target.Port != "NET3" || snap.Target.Name != "NET3NAME" {
		t.Fatalf("target = %+v", snap.Target)
	}
	if len(snap.ButtonLabels) == 0 || snap.ButtonLabels[0] != "KAM1" {
		t.Fatalf("button labels = %#v", snap.ButtonLabels)
	}
	cancel()
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
}

func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "kroma-helper-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "h.sock")
}

func waitHelperHealth(t *testing.T, client *Client) {
	t.Helper()
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
	t.Fatalf("helper did not become healthy: %v", lastErr)
}

func startFakeMatrixServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		for {
			payload, err := matrix.ReadWirePayload(r)
			if err != nil {
				return
			}
			if string(payload) == string(matrix.ACKPayload) {
				continue
			}
			switch {
			case string(payload) == string([]byte{0x00, 0x49, 0x0E, 0x01, 0x00}):
				_, _ = conn.Write(matrix.BuildWireFrame(append([]byte{0x00, 0x49, 0x0E, 0x01, 0x00}, []byte("TH5012")...)))
			case string(payload) == string([]byte{0x00, 0x49, 0x00}):
				_, _ = conn.Write(matrix.BuildWireFrame(append([]byte{0x00, 0x49, 0x01, 0x01}, []byte("Aktualna")...)))
			case string(payload) == string([]byte{0x00, 0x49, 0x03, 0x01}):
				data := fakeMapData()
				size := []byte{0x00, 0x49, 0x04, 0, 0, 0, 0}
				binary.BigEndian.PutUint32(size[3:], uint32(len(data)))
				_, _ = conn.Write(matrix.BuildWireFrame(size))
				chunk := append([]byte{0x00, 0x49, 0x05, 0x00}, data...)
				_, _ = conn.Write(matrix.BuildWireFrame(chunk))
				_, _ = conn.Write(matrix.BuildWireFrame([]byte{0x00, 0x49, 0x06}))
			}
		}
	}()
	return ln.Addr().String()
}

func fakeMapData() []byte {
	data := []byte{'K', 'M', 'P', 0x02}
	data = append(data, fakeC2Record(11, "NET3NAME", fakeCCRecord(0, "KAM1"))...)
	data = append(data, 0xFF)
	return data
}

func fakeC2Record(port int, name string, records ...[]byte) []byte {
	body := []byte{0x20, byte(port), 0x21, 0x06}
	body = append(body, fakeTLVString(name)...)
	for _, record := range records {
		body = append(body, record...)
	}
	return fakeLenRecord(0xC2, body)
}

func fakeCCRecord(key int, label string) []byte {
	body := []byte{0x20, byte(key)}
	body = append(body, fakeTLVString(label)...)
	return fakeLenRecord(0xCC, body)
}

func fakeTLVString(s string) []byte {
	out := []byte{0x82, byte(len(s))}
	return append(out, []byte(s)...)
}

func fakeLenRecord(tag byte, body []byte) []byte {
	out := []byte{tag, byte(len(body)), byte(len(body) >> 8)}
	return append(out, body...)
}
