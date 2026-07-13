package prtp

import "testing"

func TestCRCAndroidVectors(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
		crc     byte
	}{
		{name: "ack", payload: []byte{0x41}, crc: 0x4B},
		{name: "nack", payload: []byte{0x4E}, crc: 0xDD},
		{name: "sync", payload: []byte{0x53}, crc: 0xC5},
		{name: "server ping", payload: []byte{0x69, 0x50}, crc: 0x33},
		{name: "server R", payload: []byte{0x69, 0x52, 0x01}, crc: 0xD1},
		{name: "server identity", payload: []byte{0x69, 0x49, 0xD0}, crc: 0xBB},
		{name: "server info", payload: []byte{0x69, 0x49, 0x84, 0x00, 0x00, 0x00, 0x00}, crc: 0x0B},
		{name: "key press", payload: []byte{0x49, 0x00, 0x80}, crc: 0x63},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CRC(tt.payload); got != tt.crc {
				t.Fatalf("CRC(% X) = %02X, want %02X", tt.payload, got, tt.crc)
			}
			frame := append(append([]byte(nil), tt.payload...), tt.crc)
			if got := CRCResidue(frame); got != 0 {
				t.Fatalf("CRCResidue(% X) = %02X, want 00", frame, got)
			}
		})
	}
}

func TestNormalizePayload(t *testing.T) {
	got, prefix, ok := NormalizePayload([]byte{0x69, 0x50})
	if !ok || prefix != 0x69 || string(got) != string([]byte{0x50}) {
		t.Fatalf("NormalizePayload returned payload=% X prefix=%02X ok=%v", got, prefix, ok)
	}
	got, _, ok = NormalizePayload([]byte{0x49, 0x00, 0x80})
	if ok || string(got) != string([]byte{0x49, 0x00, 0x80}) {
		t.Fatalf("NormalizePayload client payload=% X ok=%v", got, ok)
	}
	got, prefix, ok = NormalizePayload([]byte{0x69, 0x52, 0x01})
	if !ok || prefix != 0x69 || string(got) != string([]byte{0x52, 0x01}) {
		t.Fatalf("NormalizePayload server R payload=% X prefix=%02X ok=%v", got, prefix, ok)
	}
}

func TestDecodeFrameRejectsBadCRC(t *testing.T) {
	payload := []byte{0x49, 0x90, 0x03, 'A'}
	wire := BuildFrame(payload)
	frame := append([]byte(nil), wire[:len(wire)-1]...)
	got, ok := DecodeFrame(frame)
	if !ok || string(got) != string(payload) {
		t.Fatalf("DecodeFrame valid = % X, %v; want % X, true", got, ok, payload)
	}
	frame[len(frame)-1] ^= 0x01
	if got, ok := DecodeFrame(frame); ok || got != nil {
		t.Fatalf("DecodeFrame accepted bad CRC: % X, %v", got, ok)
	}
}

func TestBuildAudioPacket(t *testing.T) {
	payload := make([]byte, AudioPayloadSize)
	for i := range payload {
		payload[i] = byte(i)
	}
	packet := BuildAudioPacket(0x12, payload, []byte{0x41, 0xFF, 0x53, 0x00, 0x99})
	if !IsAudioPacket(packet) || len(packet) != AudioFrameSize {
		t.Fatalf("packet shape invalid len=%d header=% X", len(packet), packet[:12])
	}
	if packet[5] != 0x27 || packet[7] != 4 {
		t.Fatalf("control header = % X", packet[:12])
	}
	if string(packet[8:12]) != string([]byte{0x41, 0xFF, 0x53, 0x00}) {
		t.Fatalf("control bytes = % X", packet[8:12])
	}
}

func TestEmulationProfileAndIdentity(t *testing.T) {
	p, err := NewEmulationProfile("tp5024", "TP50241234", 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.Model != "TP5024" || p.TypeCode != TypeTP5024 || p.KeyCount != 24 {
		t.Fatalf("profile = %+v", p)
	}
	payload := IdentityPayload(p)
	want := []byte{0x49, 0xD0, 0x1E, 'T', 'P', '5', '0', '2', '4', '1', '2', '3', '4', 0x01, 0x03, 0x00, 0x00, 0x00}
	if string(payload) != string(want) {
		t.Fatalf("identity payload = % X, want % X", payload, want)
	}
	if got := ParseIdentityText(payload); got != "TP50241234" {
		t.Fatalf("ParseIdentityText = %q", got)
	}
}

func TestEmulationDeviceForTypeCode(t *testing.T) {
	tests := []struct {
		code int
		want string
		ok   bool
	}{
		{code: int(TypeBP7100), want: "bp7100", ok: true},
		{code: int(TypeTP5012), want: "tp5012", ok: true},
		{code: int(TypeTP5024), want: "tp5024", ok: true},
		{code: int(TypeTP5008), want: "tp5008", ok: true},
		{code: 0x1A, ok: false},
	}
	for _, tt := range tests {
		got, ok := EmulationDeviceForTypeCode(tt.code)
		if got != tt.want || ok != tt.ok {
			t.Fatalf("EmulationDeviceForTypeCode(0x%02X) = %q, %v; want %q, %v", tt.code, got, ok, tt.want, tt.ok)
		}
	}
}
