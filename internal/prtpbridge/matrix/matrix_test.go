package matrix

import (
	"bufio"
	"bytes"
	"net"
	"testing"
	"time"
)

func TestWireFrameVectors(t *testing.T) {
	if got := BuildWireFrame([]byte{0x00, 0x41}); string(got) != string([]byte{0x00, 0x41, 0x4B, 0xFF}) {
		t.Fatalf("ACK frame = % X", got)
	}
	if got := BuildWireFrame([]byte{0x00, 0x49, 0x0E, 0x01, 0x00}); string(got) != string([]byte{0x00, 0x49, 0x0E, 0x01, 0x00, 0xE9, 0xFF}) {
		t.Fatalf("handshake frame = % X", got)
	}
	payload := []byte{0x00, 0x49, 0x0E, 0x01, 0x00, 0xFF, 0xFE}
	frame := BuildWireFrame(payload)
	if !bytes.Contains(frame, []byte{0xFE, 0xFF}) || !bytes.Contains(frame, []byte{0xFE, 0xFE}) {
		t.Fatalf("escaped frame = % X", frame)
	}
	got, err := ReadWirePayload(bufio.NewReader(bytes.NewReader(frame)))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Fatalf("ReadWirePayload = % X, want % X", got, payload)
	}
}

func TestParsePortRef(t *testing.T) {
	tests := []struct {
		in        string
		wantPort  string
		wantIndex int
	}{
		{in: "NET0", wantPort: "NET0", wantIndex: 8},
		{in: "net3", wantPort: "NET3", wantIndex: 11},
		{in: "ANAG2", wantPort: "ANAG2", wantIndex: 6},
		{in: "HEAD1", wantPort: "HEAD1", wantIndex: 13},
	}
	for _, tt := range tests {
		got, err := ParsePortRef(tt.in)
		if err != nil {
			t.Fatal(err)
		}
		if got.Port != tt.wantPort || got.Index != tt.wantIndex {
			t.Fatalf("ParsePortRef(%q) = %+v", tt.in, got)
		}
	}
	if _, err := ParsePortRef("NET4"); err == nil {
		t.Fatal("ParsePortRef(NET4) succeeded")
	}
}

func TestParseBinaryMapDataKeepsLabelsByPort(t *testing.T) {
	data := []byte{'K', 'M', 'P', 0x02, 0x0D, 0x26, 0xE0, 0x2A, 0x08, 0x00, 0x00, 0x20, 0x00}
	data = append(data, testC2Record(0, "INZYNIER",
		testCCRecord(14, "DIG0-14"),
		testCCRecord(15, "DIG0-15"),
	)...)
	data = append(data, testC2Record(1, "REALIZATOR",
		testCCRecord(0, "DIG1-0"),
		testCCRecord(2, "DIG1-2"),
	)...)
	data = append(data, 0xFF)

	parsed := ParseMapData(data)
	if parsed.Version != 2 || len(parsed.Ports) != 2 {
		t.Fatalf("parsed = %+v", parsed)
	}
	if port, ok := PortByIndex(parsed.Ports, 0); !ok || port.Name != "INZYNIER" {
		t.Fatalf("DIG0 port = %+v ok=%v", port, ok)
	}
	if port, ok := PortByIndex(parsed.Ports, 1); !ok || port.TypeCode != 0x06 || port.TypeName != "TP7016" {
		t.Fatalf("DIG1 type = %+v ok=%v", port, ok)
	}
	dig1 := ButtonLabelsForPort(parsed.Labels, PortName{Index: 1, Port: "DIG1", Name: "REALIZATOR"}, 16)
	want := []string{"DIG1-0", "", "DIG1-2"}
	if len(dig1) != len(want) {
		t.Fatalf("DIG1 labels = %#v", dig1)
	}
	for i := range want {
		if dig1[i] != want[i] {
			t.Fatalf("DIG1 labels = %#v, want %#v", dig1, want)
		}
	}
}

func TestFetchCurrentMapSkipsStaleChunksBeforeSize(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	errCh := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			errCh <- err
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		if p, err := ReadWirePayload(r); err != nil || !bytes.Equal(p, []byte{0x00, 0x49, 0x00}) {
			errCh <- err
			return
		}
		if err := WriteWirePayload(c, []byte{0x00, 0x49, 0x05, 0x01, 's', 't', 'a', 'l', 'e'}); err != nil {
			errCh <- err
			return
		}
		names := make([]byte, 24)
		copy(names[16:], []byte("Aktualna"))
		bank := append([]byte{0x00, 0x49, 0x01, 0x03}, names...)
		if err := WriteWirePayload(c, bank); err != nil {
			errCh <- err
			return
		}
		for {
			p, err := ReadWirePayload(r)
			if err != nil {
				errCh <- err
				return
			}
			if bytes.Equal(p, []byte{0x00, 0x49, 0x03, 0x03}) {
				break
			}
		}
		for _, p := range [][]byte{
			{0x00, 0x49, 0x05, 0x02, 'o', 'l', 'd'},
			{0x00, 0x49, 0x06},
			{0x00, 0x49, 0x04, 0x00, 0x00, 0x00, 0x06},
			{0x00, 0x49, 0x05, 0x00, 'a', 'b', 'c'},
			{0x00, 0x49, 0x05, 0x01, 'd', 'e', 'f'},
			{0x00, 0x49, 0x06},
		} {
			if err := WriteWirePayload(c, p); err != nil {
				errCh <- err
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
		errCh <- nil
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	got, err := FetchCurrentMap(conn, bufio.NewReader(conn), time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Bank != 3 || got.BankName != "Aktualna" || got.Size != 6 || string(got.Data) != "abcdef" {
		t.Fatalf("map download = bank %d name %q size %d data %q", got.Bank, got.BankName, got.Size, string(got.Data))
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
}

func testC2Record(port int, name string, records ...[]byte) []byte {
	body := []byte{0x20, byte(port), 0x21, 0x06}
	body = append(body, testTLVString(name)...)
	for _, record := range records {
		body = append(body, record...)
	}
	return testLenRecord(0xC2, body)
}

func testCCRecord(key int, label string) []byte {
	body := []byte{0x20, byte(key)}
	body = append(body, testTLVString(label)...)
	body = append(body, 0xC3, 0x04, 0x00, 0x20, 0x00, 0x22, byte(key))
	return testLenRecord(0xCC, body)
}

func testTLVString(s string) []byte {
	out := []byte{0x82, byte(len(s))}
	return append(out, []byte(s)...)
}

func testLenRecord(tag byte, body []byte) []byte {
	out := []byte{tag, byte(len(body)), byte(len(body) >> 8)}
	return append(out, body...)
}
