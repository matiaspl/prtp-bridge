package prtp

import (
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
)

const (
	AudioFrameSize     = 0x10c
	AudioPayloadOffset = 12
	AudioPayloadSize   = 256
)

func CRC(payload []byte) byte {
	if len(payload) == 0 {
		return 0
	}
	frame := make([]byte, len(payload)+1)
	copy(frame, payload)
	return CRCResidue(frame)
}

func CRCResidue(frame []byte) byte {
	if len(frame) == 0 {
		return 0
	}
	res := frame[0]
	for i := 0; i < len(frame)-1; i++ {
		for bit := 0; bit < 8; {
			if (res & 0x80) == 0 {
				next := (frame[i+1] >> uint(7-bit)) & 0x01
				res = (res << 1) | next
				bit++
			}
			res ^= 0x8D
		}
	}
	return res
}

func IsMessageType(b byte) bool {
	switch b {
	case 0x41, 0x43, 0x49, 0x4E, 0x50, 0x52, 0x53:
		return true
	default:
		return false
	}
}

func NormalizePayload(raw []byte) (payload []byte, prefix byte, hasPrefix bool) {
	if len(raw) >= 2 && !IsMessageType(raw[0]) && IsMessageType(raw[1]) {
		return raw[1:], raw[0], true
	}
	return raw, 0, false
}

func BuildFrame(payload []byte) []byte {
	frame := make([]byte, len(payload)+1)
	copy(frame, payload)
	frame[len(payload)] = CRC(payload)
	out := make([]byte, 0, len(frame)+2)
	for _, b := range frame {
		if b == 0xFE || b == 0xFF {
			out = append(out, 0xFE)
		}
		out = append(out, b)
	}
	out = append(out, 0xFF)
	return out
}

func DecodeFrame(frame []byte) (payload []byte, ok bool) {
	if len(frame) < 2 || CRCResidue(frame) != 0 {
		return nil, false
	}
	payload, _, _ = NormalizePayload(frame[:len(frame)-1])
	return payload, true
}

func BuildAudioPacketInto(d []byte, seq byte, payload []byte, ctrl []byte) []byte {
	if cap(d) < AudioFrameSize {
		d = make([]byte, AudioFrameSize)
	} else {
		d = d[:AudioFrameSize]
	}
	for i := 0; i < AudioPayloadOffset; i++ {
		d[i] = 0
	}
	d[0] = 0xAA
	d[1] = seq
	d[2] = 0x01
	d[3] = 0x0C
	d[4] = 0x00
	d[5] = 0x23
	d[6] = 0x03
	if len(ctrl) > 0 {
		if len(ctrl) > 4 {
			ctrl = ctrl[:4]
		}
		d[5] = 0x27
		d[7] = byte(len(ctrl))
		copy(d[8:], ctrl)
	}
	n := len(payload)
	if n > AudioPayloadSize {
		n = AudioPayloadSize
	}
	copy(d[AudioPayloadOffset:], payload[:n])
	for i := n; i < AudioPayloadSize; i++ {
		d[AudioPayloadOffset+i] = 0
	}
	return d
}

func BuildAudioPacket(seq byte, payload []byte, ctrl []byte) []byte {
	return BuildAudioPacketInto(nil, seq, payload, ctrl)
}

func IsAudioPacket(packet []byte) bool {
	return len(packet) == AudioFrameSize && packet[0] == 0xAA && packet[3] == 0x0C
}

type EmulationProfile struct {
	Model       string
	Kind        string
	Name        string
	TypeCode    byte
	KeyCount    int
	Labels      []string
	UserVersion [4]byte
	FPGAVersion byte
}

type EmulationState struct {
	announced uint32
}

func (s *EmulationState) AnnounceOnce() bool {
	if s == nil {
		return false
	}
	return atomic.CompareAndSwapUint32(&s.announced, 0, 1)
}

const (
	VirtualPanelTypeCode byte = 0x2F

	TypeBP7100 byte = 0x1C
	TypeTP5012 byte = 0x1D
	TypeTP5024 byte = 0x1E
	TypeTP5008 byte = 0x21
)

func NewEmulationProfile(device, name string, keyOverride int) (*EmulationProfile, error) {
	token := strings.ToLower(strings.TrimSpace(device))
	token = strings.NewReplacer("-", "", "_", "", " ", "").Replace(token)
	if token == "" || token == "none" || token == "off" || token == "false" {
		return nil, nil
	}

	p := &EmulationProfile{
		UserVersion: [4]byte{0x01, 0x03, 0x00, 0x00},
		FPGAVersion: 0x00,
	}
	switch token {
	case "tp5008", "5008":
		p.Model = "TP5008"
		p.Kind = "panel"
		p.TypeCode = TypeTP5008
		p.KeyCount = 8
	case "tp5012", "5012":
		p.Model = "TP5012"
		p.Kind = "panel"
		p.TypeCode = TypeTP5012
		p.KeyCount = 12
	case "tp5024", "5024":
		p.Model = "TP5024"
		p.Kind = "panel"
		p.TypeCode = TypeTP5024
		p.KeyCount = 24
	case "bp7100", "7100":
		p.Model = "BP7100"
		p.Kind = "beltpack"
		p.TypeCode = TypeBP7100
		p.KeyCount = 0
	default:
		return nil, fmt.Errorf("unknown emulated PRTP endpoint %q (expected tp5008, tp5012, tp5024, or bp7100)", device)
	}

	if keyOverride < 0 || keyOverride > 120 {
		return nil, fmt.Errorf("simulate keys must be in range 0-120")
	}
	if keyOverride > 0 && p.TypeCode != TypeBP7100 {
		p.KeyCount = keyOverride
	}
	if p.KeyCount <= 0 && p.TypeCode != TypeBP7100 {
		return nil, fmt.Errorf("emulated PRTP endpoint must expose at least one key")
	}
	if strings.TrimSpace(name) == "" {
		p.Name = p.Model
	} else {
		p.Name = strings.TrimSpace(name)
	}
	p.Labels = DefaultEmulationLabels(p.Model, p.KeyCount)
	return p, nil
}

func EmulationDeviceForTypeCode(code int) (string, bool) {
	switch byte(code) {
	case TypeBP7100:
		return "bp7100", true
	case TypeTP5012:
		return "tp5012", true
	case TypeTP5024:
		return "tp5024", true
	case TypeTP5008:
		return "tp5008", true
	default:
		return "", false
	}
}

func ApplyEmulationVersions(p *EmulationProfile, userVersion, fpgaVersion string) error {
	if p == nil {
		return nil
	}
	uv, err := ParseUserVersion(userVersion)
	if err != nil {
		return err
	}
	fv, err := ParseFPGAVersion(fpgaVersion)
	if err != nil {
		return err
	}
	p.UserVersion = uv
	p.FPGAVersion = fv
	return nil
}

func ParseUserVersion(s string) ([4]byte, error) {
	var out [4]byte
	s = normalizeVersionString(s)
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return out, fmt.Errorf("user version must be major.minor.patch")
	}
	major, err := parseBoundedVersionPart(parts[0], 255, "user version major")
	if err != nil {
		return out, err
	}
	minor, err := parseBoundedVersionPart(parts[1], 255, "user version minor")
	if err != nil {
		return out, err
	}
	patch, err := parseBoundedVersionPart(parts[2], 65535, "user version patch")
	if err != nil {
		return out, err
	}
	out[0] = byte(major)
	out[1] = byte(minor)
	out[2] = byte(patch >> 8)
	out[3] = byte(patch)
	return out, nil
}

func ParseFPGAVersion(s string) (byte, error) {
	s = normalizeVersionString(s)
	parts := strings.Split(s, ".")
	if len(parts) != 2 {
		return 0, fmt.Errorf("fpga version must be major.minor")
	}
	major, err := parseBoundedVersionPart(parts[0], 15, "fpga version major")
	if err != nil {
		return 0, err
	}
	minor, err := parseBoundedVersionPart(parts[1], 15, "fpga version minor")
	if err != nil {
		return 0, err
	}
	return byte((major << 4) | minor), nil
}

func normalizeVersionString(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 0 && (s[0] == 'v' || s[0] == 'V') {
		s = strings.TrimSpace(s[1:])
		if strings.HasPrefix(s, ".") {
			s = strings.TrimSpace(s[1:])
		}
	}
	return s
}

func parseBoundedVersionPart(s string, max int, name string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("%s is empty", name)
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > max {
		return 0, fmt.Errorf("%s must be in range 0-%d", name, max)
	}
	return n, nil
}

func FormatUserVersion(v [4]byte) string {
	patch := (int(v[2]) << 8) | int(v[3])
	return fmt.Sprintf("v.%d.%02d.%03d", v[0], v[1], patch)
}

func FormatFPGAVersion(v byte) string {
	return fmt.Sprintf("v.%d.%d", v>>4, v&0x0F)
}

func DefaultEmulationLabels(model string, keyCount int) []string {
	labels := make([]string, keyCount)
	if model == "BP7100" {
		for i := range labels {
			switch i {
			case 0:
				labels[i] = "A"
			case 1:
				labels[i] = "B"
			case 2:
				labels[i] = "CALL"
			case 3:
				labels[i] = "REPLY"
			default:
				labels[i] = fmt.Sprintf("K%02d", i+1)
			}
		}
		return labels
	}
	for i := range labels {
		labels[i] = fmt.Sprintf("K%02d", i+1)
	}
	return labels
}

func BitmapGroupCount(keyCount int) int {
	if keyCount <= 0 {
		return 0
	}
	return (keyCount + 7) / 8
}

func EmulationIdentityCode(p *EmulationProfile) string {
	name := ""
	if p != nil {
		name = strings.TrimSpace(p.Name)
		if name == "" {
			name = p.Model
		}
	}
	if name == "" {
		name = "PRTP"
	}
	var sb strings.Builder
	for _, r := range name {
		if r < 0x20 || r > 0x7E {
			sb.WriteByte('_')
			continue
		}
		sb.WriteByte(byte(r))
		if sb.Len() == 10 {
			break
		}
	}
	for sb.Len() < 10 {
		sb.WriteByte(' ')
	}
	return sb.String()
}

func IdentityTypeCode(p *EmulationProfile) byte {
	if p == nil || p.TypeCode == 0 {
		return VirtualPanelTypeCode
	}
	return p.TypeCode
}

func IdentityPayload(p *EmulationProfile) []byte {
	userVersion := [4]byte{0x01, 0x03, 0x00, 0x00}
	fpgaVersion := byte(0)
	if p != nil {
		userVersion = p.UserVersion
		fpgaVersion = p.FPGAVersion
	}
	payload := []byte{0x49, 0xD0, IdentityTypeCode(p)}
	payload = append(payload, []byte(EmulationIdentityCode(p))...)
	payload = append(payload, userVersion[:]...)
	payload = append(payload, fpgaVersion)
	return payload
}

func EmulationPayloads(p *EmulationProfile) [][]byte {
	if p == nil {
		return nil
	}
	return [][]byte{IdentityPayload(p)}
}

func ParseIdentityText(payload []byte) string {
	if len(payload) >= 18 && payload[0] == 0x49 && payload[1] == 0xD0 {
		return strings.TrimRight(string(payload[3:13]), " \x00")
	}
	if len(payload) >= 3 && payload[0] == 0x49 && payload[1] == 0xD0 && (payload[2]&0xF0) == 0x20 {
		tailLen := int(payload[2] & 0x0F)
		start := 3
		end := start + tailLen
		if end > len(payload) {
			end = len(payload)
		}
		textEnd := end
		if tailLen >= 5 {
			textEnd = end - 5
		}
		if textEnd < start {
			textEnd = start
		}
		return strings.TrimRight(string(payload[start:textEnd]), " \x00")
	}
	if len(payload) > 2 {
		return string(payload[2:])
	}
	return ""
}

func HexBytes(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, v := range b {
		if i > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteString(fmt.Sprintf("%02X", v))
	}
	return sb.String()
}
