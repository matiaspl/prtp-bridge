package matrix

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"time"

	"prtp-bridge/internal/prtpbridge/prtp"
)

const (
	DefaultPort            = "2222"
	TCPReaderSize          = 32 * 1024
	DefaultConnectTimeout  = 5 * time.Second
	DefaultExchangeTimeout = 900 * time.Millisecond
)

var (
	ACKPayload = []byte{0x00, 0x41}
	ACKFrame   = BuildWireFrame(ACKPayload)

	ErrCrosspointNotImplemented = errors.New("native TCP/2222 crosspoint write is not implemented")
)

var PortIDs = []string{
	"DIG0", "DIG1", "DIG2", "DIG3",
	"ANAG0", "ANAG1", "ANAG2", "ANAG3",
	"NET0", "NET1", "NET2", "NET3",
	"HEAD0", "HEAD1",
}

type PortName struct {
	Index    int    `json:"index"`
	Port     string `json:"port"`
	Name     string `json:"name"`
	TypeCode int    `json:"type_code,omitempty"`
	TypeName string `json:"type_name,omitempty"`
}

type PortRef struct {
	Index int    `json:"index"`
	Port  string `json:"port"`
}

type ButtonLabel struct {
	PortIndex int    `json:"port_index"`
	Port      string `json:"port"`
	Key       int    `json:"key"`
	Label     string `json:"label"`
}

type NameSnapshot struct {
	Addr         string        `json:"addr"`
	Device       string        `json:"device,omitempty"`
	Target       PortName      `json:"target"`
	Ports        []PortName    `json:"ports"`
	ButtonLabels []string      `json:"button_labels,omitempty"`
	MapLabels    []ButtonLabel `json:"map_labels,omitempty"`
	MapBank      int           `json:"map_bank,omitempty"`
	MapBankName  string        `json:"map_bank_name,omitempty"`
	MapSize      int           `json:"map_size,omitempty"`
	MapError     string        `json:"map_error,omitempty"`
	Strings      []string      `json:"strings,omitempty"`
}

type MapDownload struct {
	Bank     int
	BankName string
	Size     int
	Data     []byte
}

type MapParseResult struct {
	Version int
	Strings []string
	Ports   []PortName
	Labels  []ButtonLabel
}

type CrosspointRequest struct {
	XIn     int  `json:"xin"`
	XOut    int  `json:"xout"`
	Enabled bool `json:"enabled"`
	Save    bool `json:"save"`
}

func NormalizeAddr(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	}
	return net.JoinHostPort(addr, DefaultPort)
}

func ParsePortRef(s string) (PortRef, error) {
	token := strings.ToUpper(strings.TrimSpace(s))
	token = strings.NewReplacer("-", "", "_", "", " ", "").Replace(token)
	if token == "" {
		return PortRef{}, errors.New("matrix port is required")
	}
	for i, name := range PortIDs {
		if token == name {
			return PortRef{Index: i, Port: name}, nil
		}
	}
	groups := []struct {
		prefix string
		base   int
		count  int
	}{
		{prefix: "DIG", base: 0, count: 4},
		{prefix: "ANAG", base: 4, count: 4},
		{prefix: "NET", base: 8, count: 4},
		{prefix: "HEAD", base: 12, count: 2},
	}
	for _, group := range groups {
		if !strings.HasPrefix(token, group.prefix) {
			continue
		}
		n, err := strconv.Atoi(strings.TrimPrefix(token, group.prefix))
		if err != nil {
			break
		}
		if n < 0 || n >= group.count {
			return PortRef{}, fmt.Errorf("matrix port %q is outside known range %s0-%s%d", s, group.prefix, group.prefix, group.count-1)
		}
		idx := group.base + n
		return PortRef{Index: idx, Port: PortIDs[idx]}, nil
	}
	return PortRef{}, fmt.Errorf("invalid matrix port %q (expected DIG0..DIG3, ANAG0..ANAG3, NET0..NET3, or HEAD0..HEAD1)", s)
}

func PortTypeName(code int) string {
	switch code {
	case 0x01:
		return "TP6008"
	case 0x02:
		return "TP6024"
	case 0x03:
		return "CP6032"
	case 0x04:
		return "AP6000"
	case 0x05:
		return "BP6000"
	case 0x06:
		return "TP7016"
	case 0x07:
		return "HN6000"
	case 0x08:
		return "TP7010"
	case 0x09:
		return "TA7000"
	case 0x0A:
		return "TP7100"
	case 0x0F:
		return "TP7210"
	case 0x11:
		return "GP7020"
	case 0x15:
		return "TW7100"
	case 0x16:
		return "CE6000"
	case 0x17:
		return "EL6000"
	case 0x18:
		return "EL6001"
	case 0x19:
		return "IR6000"
	case 0x1A:
		return "AUDIO"
	case 0x1B:
		return "HEADSET"
	case 0x1C:
		return "BP7100"
	case 0x1D:
		return "TP5012"
	case 0x1E:
		return "TP5024"
	case 0x21:
		return "TP5008"
	default:
		return ""
	}
}

func BuildWireFrame(payload []byte) []byte {
	if len(payload) == 0 {
		return nil
	}
	frame := make([]byte, len(payload)+1)
	copy(frame, payload)
	frame[len(payload)] = prtp.CRC(payload)
	out := make([]byte, 0, len(frame)+2)
	out = append(out, frame[0])
	for _, b := range frame[1:] {
		if b == 0xFE || b == 0xFF {
			out = append(out, 0xFE)
		}
		out = append(out, b)
	}
	out = append(out, 0xFF)
	return out
}

func ReadWirePayload(r *bufio.Reader) ([]byte, error) {
	var frame []byte
	esc := false
	for {
		b, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		if esc {
			frame = append(frame, b)
			esc = false
			continue
		}
		switch b {
		case 0xFE:
			esc = true
		case 0xFF:
			if len(frame) < 2 {
				frame = frame[:0]
				continue
			}
			if got := prtp.CRCResidue(frame); got != 0 {
				return nil, fmt.Errorf("bad Kroma wire CRC residue 0x%02X for [%s]", got, prtp.HexBytes(frame))
			}
			return frame[:len(frame)-1], nil
		default:
			frame = append(frame, b)
		}
	}
}

func ConfigureTCPConn(conn net.Conn, debug bool) {
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		return
	}
	if err := tcp.SetNoDelay(true); err != nil {
		log.Printf("matrix tcp nodelay setup failed: %v", err)
	} else if debug {
		log.Printf("matrix tcp nodelay enabled")
	}
}

func WriteWirePayload(conn net.Conn, payload []byte) error {
	frame := BuildWireFrame(payload)
	if len(frame) == 0 {
		return nil
	}
	_, err := conn.Write(frame)
	return err
}

func WriteACK(conn net.Conn) error {
	_, err := conn.Write(ACKFrame)
	return err
}

func readPayload(conn net.Conn, r *bufio.Reader, deadline time.Time) ([]byte, error) {
	_ = conn.SetReadDeadline(deadline)
	return ReadWirePayload(r)
}

func Exchange(conn net.Conn, r *bufio.Reader, debug bool, label string, request []byte, match func([]byte) bool, timeout time.Duration) ([]byte, error) {
	logTX(debug, label, request)
	if err := WriteWirePayload(conn, request); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		payload, err := readPayload(conn, r, deadline)
		if err != nil {
			log.Printf("matrix tcp rx %s error: %v", label, err)
			return nil, err
		}
		if len(payload) >= 2 && payload[0] == 0x00 && payload[1] == 0x49 {
			if err := WriteACK(conn); err != nil {
				return nil, err
			}
			logTX(debug, "ack", ACKPayload)
		}
		logRX(debug, label, payload)
		if match == nil || match(payload) {
			return payload, nil
		}
	}
	log.Printf("matrix tcp %s timeout after %s", label, timeout)
	return nil, errors.New("matrix response timeout")
}

func FetchNames(addr string, target PortRef, connectTimeout, exchangeTimeout time.Duration, debug bool) (*NameSnapshot, error) {
	addr = NormalizeAddr(addr)
	if addr == "" {
		return nil, errors.New("matrix address is required")
	}
	if connectTimeout <= 0 {
		connectTimeout = DefaultConnectTimeout
	}
	if exchangeTimeout <= 0 {
		exchangeTimeout = DefaultExchangeTimeout
	}
	conn, err := net.DialTimeout("tcp", addr, connectTimeout)
	if err != nil {
		return nil, fmt.Errorf("matrix tcp connect to %s failed after %s: %w", addr, connectTimeout, err)
	}
	defer conn.Close()
	ConfigureTCPConn(conn, debug)
	log.Printf("matrix tcp connected addr=%s target=%s(%d) connect_timeout=%s exchange_timeout=%s", addr, target.Port, target.Index, connectTimeout, exchangeTimeout)

	r := bufio.NewReaderSize(conn, TCPReaderSize)
	snap := &NameSnapshot{Addr: addr, Target: PortName{Index: target.Index, Port: target.Port}}

	hello, err := Exchange(conn, r, debug, "identity", []byte{0x00, 0x49, 0x0E, 0x01, 0x00}, func(p []byte) bool {
		return len(p) >= 4 && p[0] == 0x00 && p[1] == 0x49 && p[2] == 0x0E && p[3] == 0x01
	}, exchangeTimeout)
	if err == nil {
		snap.Device = LongestPrintableASCII(hello, 6, 16)
		log.Printf("matrix tcp identity device=%q", snap.Device)
	} else {
		log.Printf("matrix tcp identity failed: %v", err)
	}

	var haveMap bool
	var mapParsedVersion int
	log.Printf("matrix tcp read current map for port names and labels")
	if mapDL, err := FetchCurrentMap(conn, r, 5*time.Second, debug); err == nil && mapDL != nil {
		snap.MapBank = mapDL.Bank
		snap.MapBankName = mapDL.BankName
		snap.MapSize = mapDL.Size
		parsed := ParseMapData(mapDL.Data)
		mapParsedVersion = parsed.Version
		snap.Strings = parsed.Strings
		snap.Ports = parsed.Ports
		snap.MapLabels = parsed.Labels
		haveMap = true
		if pn, ok := PortByIndex(snap.Ports, target.Index); ok {
			snap.Target = pn
		}
		if labels := ButtonLabelsForPort(parsed.Labels, snap.Target, 32); len(labels) > 0 {
			snap.ButtonLabels = labels
		}
		log.Printf("matrix tcp map parsed bank=%d name=%q map_version=%d advertised_size=%d downloaded=%d strings=%d ports=%d labels=%d target_labels=%d",
			mapDL.Bank, mapDL.BankName, parsed.Version, mapDL.Size, len(mapDL.Data), len(parsed.Strings), len(snap.Ports), len(parsed.Labels), len(snap.ButtonLabels))
	} else if err != nil {
		snap.MapError = err.Error()
		log.Printf("matrix tcp map read failed: %v", err)
	}

	if haveMap && mapParsedVersion == 0 && len(snap.ButtonLabels) == 0 && len(snap.Strings) > 0 && len(snap.MapLabels) == 0 {
		snap.ButtonLabels = likelyButtonLabels(snap.Strings, snap.Ports, snap.Target, 32)
		log.Printf("matrix tcp map fallback labels=%d target=%s name=%q", len(snap.ButtonLabels), snap.Target.Port, snap.Target.Name)
	}

	return snap, nil
}

func FetchCurrentMap(conn net.Conn, r *bufio.Reader, timeout time.Duration, debug bool) (*MapDownload, error) {
	bankInfo, err := Exchange(conn, r, debug, "map_bank_table", []byte{0x00, 0x49, 0x00}, func(p []byte) bool {
		info, ok := InfoPayload(p)
		return ok && len(info) >= 2 && info[0] == 0x01
	}, timeout)
	if err != nil {
		return nil, err
	}
	bank, bankName, ok := CurrentMapBank(bankInfo)
	if !ok {
		return nil, errors.New("matrix current map bank was not returned")
	}
	if bankName == "" {
		return nil, fmt.Errorf("matrix current map bank %d is empty", bank)
	}
	req := []byte{0x00, 0x49, 0x03, byte(bank)}
	logTX(debug, "map_download", req)
	if err := WriteWirePayload(conn, req); err != nil {
		return nil, err
	}
	log.Printf("matrix tcp map download requested bank=%d name=%q", bank, bankName)

	start := time.Now()
	var totalFrameRead time.Duration
	var totalAckWrite time.Duration
	var maxFrameRead time.Duration
	var maxAckWrite time.Duration
	out := &MapDownload{Bank: bank, BankName: bankName, Size: -1}
	var wantSeq byte
	var chunkCount int
	var sawSize bool
	for {
		deadline := time.Now().Add(timeout)
		readStart := time.Now()
		payload, err := readPayload(conn, r, deadline)
		frameRead := time.Since(readStart)
		totalFrameRead += frameRead
		if frameRead > maxFrameRead {
			maxFrameRead = frameRead
		}
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				break
			}
			log.Printf("matrix tcp rx map_download error: %v", err)
			return out, err
		}
		var ackWrite time.Duration
		if len(payload) >= 2 && payload[0] == 0x00 && payload[1] == 0x49 {
			ackStart := time.Now()
			if err := WriteACK(conn); err != nil {
				return out, err
			}
			ackWrite = time.Since(ackStart)
			logTX(debug, "ack", ACKPayload)
			totalAckWrite += ackWrite
			if ackWrite > maxAckWrite {
				maxAckWrite = ackWrite
			}
		}
		logRX(debug, "map_download", payload)
		if debug {
			log.Printf("matrix tcp map timing %s frame_read=%s ack_write=%s downloaded=%d", PayloadSummary(payload), frameRead, ackWrite, len(out.Data))
		}
		info, ok := InfoPayload(payload)
		if !ok || len(info) < 1 {
			continue
		}
		switch info[0] {
		case 0x04:
			if len(info) >= 5 {
				out.Size = int(binary.BigEndian.Uint32(info[1:5]))
				sawSize = true
				wantSeq = 0
				out.Data = out.Data[:0]
				chunkCount = 0
				if debug {
					log.Printf("matrix tcp map size announced=%d", out.Size)
				}
			}
		case 0x05:
			if !sawSize {
				if debug {
					log.Printf("matrix tcp stale map chunk ignored before size %s", PayloadSummary(payload))
				}
				continue
			}
			if len(info) < 2 {
				return out, errors.New("matrix map chunk missing sequence byte")
			}
			seq := info[1]
			switch {
			case seq == wantSeq:
				out.Data = append(out.Data, info[2:]...)
				wantSeq++
				chunkCount++
				if debug {
					log.Printf("matrix tcp map chunk accepted seq=%d chunk_bytes=%d total=%d", seq, len(info)-2, len(out.Data))
				}
			case seq == wantSeq-1:
				if debug {
					log.Printf("matrix tcp map chunk duplicate seq=%d ignored total=%d", seq, len(out.Data))
				}
			default:
				return out, fmt.Errorf("matrix map chunk sequence mismatch: got %d want %d", seq, wantSeq)
			}
		case 0x06:
			if !sawSize {
				if debug {
					log.Printf("matrix tcp stale map end ignored before size")
				}
				continue
			}
			elapsed := time.Since(start)
			rate := 0.0
			if elapsed > 0 {
				rate = float64(len(out.Data)) / elapsed.Seconds()
			}
			log.Printf("matrix tcp map download complete bank=%d chunks=%d bytes=%d announced_size=%d elapsed=%s rate=%.0fB/s frame_read=%s ack_write=%s max_frame_read=%s max_ack_write=%s",
				bank, chunkCount, len(out.Data), out.Size, elapsed, rate, totalFrameRead, totalAckWrite, maxFrameRead, maxAckWrite)
			return out, nil
		}
	}
	if len(out.Data) == 0 {
		log.Printf("matrix tcp map download timed out before data elapsed=%s frame_read=%s ack_write=%s", time.Since(start), totalFrameRead, totalAckWrite)
		return out, errors.New("matrix map download timed out before data")
	}
	log.Printf("matrix tcp map download timed out before completion bytes=%d announced_size=%d elapsed=%s frame_read=%s ack_write=%s", len(out.Data), out.Size, time.Since(start), totalFrameRead, totalAckWrite)
	return out, errors.New("matrix map download timed out before completion")
}

func SetCrosspoint(addr string, req CrosspointRequest) error {
	return ErrCrosspointNotImplemented
}

func CurrentMapBank(payload []byte) (int, string, bool) {
	info, ok := InfoPayload(payload)
	if !ok || len(info) < 2 || info[0] != 0x01 {
		return 0, "", false
	}
	bank := int(info[1])
	if bank <= 0 {
		return 0, "", false
	}
	names := info[2:]
	offset := (bank - 1) * 8
	if offset < 0 || offset >= len(names) {
		return bank, "", true
	}
	name := PrintableASCIIField(names[offset:], 8)
	return bank, name, true
}

func InfoPayload(payload []byte) ([]byte, bool) {
	if len(payload) < 3 || payload[0] != 0x00 || payload[1] != 0x49 {
		return nil, false
	}
	return payload[2:], true
}

func PortNameFromPayload(payload []byte) (PortName, bool) {
	info, ok := InfoPayload(payload)
	if !ok || len(info) < 22 || info[0] != 0x0E || info[1] != 0x00 {
		return PortName{}, false
	}
	port := int(info[2])
	if port < 0 || port >= len(PortIDs) {
		return PortName{}, false
	}
	name := PrintableASCIIField(info[21:], 20)
	if name == "" {
		name = LongestPrintableASCII(info, 3, 24)
	}
	return PortName{Index: port, Port: PortIDs[port], Name: name}, true
}

func PrintableASCIIField(b []byte, max int) string {
	if max <= 0 || max > len(b) {
		max = len(b)
	}
	var sb strings.Builder
	for i := 0; i < max; i++ {
		c := b[i]
		if c == 0x00 || c == 0xFF {
			break
		}
		if c >= 0x20 && c <= 0x7E {
			sb.WriteByte(c)
		}
	}
	return strings.TrimSpace(sb.String())
}

func LongestPrintableASCII(b []byte, minLen, maxLen int) string {
	var best, cur []byte
	flush := func() {
		if len(cur) >= minLen && len(cur) > len(best) {
			best = append(best[:0], cur...)
		}
		cur = cur[:0]
	}
	for _, c := range b {
		if c >= 0x20 && c <= 0x7E {
			if maxLen <= 0 || len(cur) < maxLen {
				cur = append(cur, c)
			}
			continue
		}
		flush()
	}
	flush()
	return strings.TrimSpace(string(best))
}

func extractTLVStrings(b []byte) []string {
	var out []string
	for i := 0; i+2 <= len(b); i++ {
		if b[i] != 0x82 {
			continue
		}
		n := int(b[i+1])
		if n <= 0 || i+2+n > len(b) {
			continue
		}
		s := PrintableASCIIField(b[i+2:i+2+n], n)
		if s != "" {
			out = append(out, s)
		}
		i += 1 + n
	}
	return out
}

func ParseMapData(data []byte) MapParseResult {
	var res MapParseResult
	if parseBinaryMap(data, &res) {
		res.Strings = UniqueStrings(res.Strings)
		res.Ports = UniquePorts(res.Ports)
		return res
	}

	off := 0
	header, ok := readCString(data, &off)
	if !ok {
		return res
	}
	res.Strings = appendString(res.Strings, header)
	switch strings.ToLower(header) {
	case "kmap1":
		res.Version = 1
	case "kmap2":
		res.Version = 2
	case "kmap3":
		res.Version = 3
	default:
		res.Strings = append(res.Strings, extractTLVStrings(data)...)
		return res
	}

	currentPort := -1
	currentType := 0
	if off < len(data) && data[off] == 0xAE {
		off++
		if off >= len(data) {
			return res
		}
		off++
		if s, ok := readCString(data, &off); ok {
			res.Strings = appendString(res.Strings, s)
		} else {
			return res
		}
	}

	for off < len(data) {
		tag := data[off]
		off++
		switch tag {
		case 0xFF:
			res.Strings = UniqueStrings(res.Strings)
			res.Ports = UniquePorts(res.Ports)
			return res
		case 0xAA:
			if off+5 > len(data) {
				return res
			}
			off += 5
			if s, ok := readCString(data, &off); ok {
				res.Strings = appendString(res.Strings, s)
			} else {
				return res
			}
		case 0xAB:
			if off+4 > len(data) {
				return res
			}
			off += 4
		case 0xAC:
			if off >= len(data) {
				return res
			}
			key := int(data[off])
			off++
			label, ok := readCString(data, &off)
			if !ok {
				return res
			}
			res.Strings = appendString(res.Strings, label)
			if currentPort >= 0 && strings.TrimSpace(label) != "" {
				portName := ""
				if currentPort < len(PortIDs) {
					portName = PortIDs[currentPort]
				}
				res.Labels = append(res.Labels, ButtonLabel{PortIndex: currentPort, Port: portName, Key: key, Label: label})
			}
		case 0xAD:
			if off+2 > len(data) {
				return res
			}
			currentPort = int(data[off])
			currentType = int(data[off+1])
			off += 2
			var portName string
			for i := 0; i < 2; i++ {
				s, ok := readCString(data, &off)
				if !ok {
					return res
				}
				res.Strings = appendString(res.Strings, s)
				if i == 0 {
					portName = s
				}
			}
			if res.Version >= 2 {
				if off >= len(data) {
					return res
				}
				off++
				for i := 0; i < 2; i++ {
					s, ok := readCString(data, &off)
					if !ok {
						return res
					}
					res.Strings = appendString(res.Strings, s)
				}
			}
			if currentPort >= 0 && currentPort < len(PortIDs) {
				res.Ports = appendPort(res.Ports, PortName{Index: currentPort, Port: PortIDs[currentPort], Name: portName, TypeCode: currentType, TypeName: PortTypeName(currentType)})
			}
		case 0xAF:
			if off+4 > len(data) {
				return res
			}
			off += 4
		case 0xB5:
			if off >= len(data) {
				return res
			}
			off++
		default:
		}
	}
	res.Strings = UniqueStrings(res.Strings)
	res.Ports = UniquePorts(res.Ports)
	return res
}

func parseBinaryMap(data []byte, res *MapParseResult) bool {
	if len(data) < 4 || data[0] != 'K' || data[1] != 'M' || data[2] != 'P' {
		return false
	}
	res.Version = int(data[3])
	res.Strings = appendString(res.Strings, fmt.Sprintf("KMP%d", res.Version))
	for _, s := range extractTLVStrings(data) {
		res.Strings = appendString(res.Strings, s)
	}

	for off := 4; off+3 <= len(data); {
		if data[off] != 0xC2 {
			off++
			continue
		}
		n := int(binary.LittleEndian.Uint16(data[off+1 : off+3]))
		if n <= 0 || off+3+n > len(data) {
			off++
			continue
		}
		parsePortRecord(data[off+3:off+3+n], res)
		off += 3 + n
	}
	return true
}

func parsePortRecord(record []byte, res *MapParseResult) {
	portIndex, ok := recordByteAttr(record, 0x20)
	if !ok {
		return
	}
	typeCode, _ := recordByteAttr(record, 0x21)
	portID := ""
	if portIndex >= 0 && portIndex < len(PortIDs) {
		portID = PortIDs[portIndex]
	}
	displayName := portID
	portHeader := record
	if i := bytes.IndexByte(record, 0xCC); i >= 0 {
		portHeader = record[:i]
	}
	if name, ok := firstTLVString(portHeader); ok && strings.TrimSpace(name) != "" {
		displayName = name
	}
	if portIndex >= 0 && portIndex < len(PortIDs) {
		res.Ports = appendPort(res.Ports, PortName{Index: portIndex, Port: portID, Name: displayName, TypeCode: typeCode, TypeName: PortTypeName(typeCode)})
	}
	for off := 0; off+3 <= len(record); {
		if record[off] != 0xCC {
			off++
			continue
		}
		n := int(binary.LittleEndian.Uint16(record[off+1 : off+3]))
		if n <= 0 || off+3+n > len(record) {
			off++
			continue
		}
		parseButtonRecord(record[off+3:off+3+n], portIndex, portID, res)
		off += 3 + n
	}
}

func parseButtonRecord(record []byte, portIndex int, portName string, res *MapParseResult) {
	key, ok := recordByteAttr(record, 0x20)
	if !ok {
		return
	}
	label, ok := firstTLVString(record)
	if !ok || strings.TrimSpace(label) == "" {
		return
	}
	res.Labels = append(res.Labels, ButtonLabel{PortIndex: portIndex, Port: portName, Key: key, Label: label})
}

func recordByteAttr(record []byte, tag byte) (int, bool) {
	limit := len(record)
	if i := bytes.IndexByte(record, 0x82); i >= 0 && i < limit {
		limit = i
	}
	if i := bytes.IndexByte(record, 0xCC); i >= 0 && i < limit {
		limit = i
	}
	for i := 0; i+1 < limit; i++ {
		if record[i] == tag {
			return int(record[i+1]), true
		}
	}
	return 0, false
}

func firstTLVString(record []byte) (string, bool) {
	for i := 0; i+2 <= len(record); i++ {
		if record[i] != 0x82 {
			continue
		}
		n := int(record[i+1])
		if n <= 0 || i+2+n > len(record) {
			continue
		}
		s := PrintableASCIIField(record[i+2:i+2+n], n)
		return s, true
	}
	return "", false
}

func readCString(data []byte, off *int) (string, bool) {
	if *off < 0 || *off >= len(data) {
		return "", false
	}
	start := *off
	for *off < len(data) && data[*off] != 0x00 {
		*off = *off + 1
	}
	if *off >= len(data) {
		return "", false
	}
	raw := data[start:*off]
	*off = *off + 1
	var sb strings.Builder
	for _, c := range raw {
		if c >= 0x20 && c <= 0x7E {
			sb.WriteByte(c)
		}
	}
	return strings.TrimSpace(sb.String()), true
}

func appendString(in []string, s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return in
	}
	return append(in, s)
}

func ButtonLabelsForPort(labels []ButtonLabel, target PortName, max int) []string {
	if max <= 0 {
		return nil
	}
	out := make([]string, max)
	seen := false
	for _, label := range labels {
		if label.PortIndex != target.Index || label.Key < 0 || label.Key >= max {
			continue
		}
		out[label.Key] = label.Label
		seen = true
	}
	if !seen {
		return nil
	}
	last := len(out) - 1
	for last >= 0 && strings.TrimSpace(out[last]) == "" {
		last--
	}
	if last < 0 {
		return nil
	}
	return out[:last+1]
}

func PortByIndex(ports []PortName, index int) (PortName, bool) {
	for _, p := range ports {
		if p.Index == index {
			return p, true
		}
	}
	return PortName{}, false
}

func appendPort(ports []PortName, port PortName) []PortName {
	if port.Index < 0 || port.Index >= len(PortIDs) {
		return ports
	}
	port.Port = PortIDs[port.Index]
	port.Name = strings.TrimSpace(port.Name)
	for i := range ports {
		if ports[i].Index != port.Index {
			continue
		}
		if ports[i].Name == "" || ports[i].Name == ports[i].Port || (port.Name != "" && port.Name != port.Port) {
			if port.TypeCode == 0 {
				port.TypeCode = ports[i].TypeCode
				port.TypeName = ports[i].TypeName
			}
			ports[i] = port
		} else if ports[i].TypeCode == 0 && port.TypeCode != 0 {
			ports[i].TypeCode = port.TypeCode
			ports[i].TypeName = port.TypeName
		}
		return ports
	}
	return append(ports, port)
}

func UniquePorts(ports []PortName) []PortName {
	byIndex := make([]PortName, len(PortIDs))
	seen := make([]bool, len(PortIDs))
	for _, p := range ports {
		if p.Index < 0 || p.Index >= len(PortIDs) {
			continue
		}
		p.Port = PortIDs[p.Index]
		p.Name = strings.TrimSpace(p.Name)
		if !seen[p.Index] || byIndex[p.Index].Name == "" || byIndex[p.Index].Name == byIndex[p.Index].Port || (p.Name != "" && p.Name != p.Port) {
			if p.TypeCode == 0 {
				p.TypeCode = byIndex[p.Index].TypeCode
				p.TypeName = byIndex[p.Index].TypeName
			}
			byIndex[p.Index] = p
			seen[p.Index] = true
		} else if byIndex[p.Index].TypeCode == 0 && p.TypeCode != 0 {
			byIndex[p.Index].TypeCode = p.TypeCode
			byIndex[p.Index].TypeName = p.TypeName
		}
	}
	out := make([]PortName, 0, len(ports))
	for i, ok := range seen {
		if ok {
			out = append(out, byIndex[i])
		}
	}
	return out
}

func likelyButtonLabels(stringsList []string, ports []PortName, target PortName, max int) []string {
	if max <= 0 {
		return nil
	}
	skip := make(map[string]struct{})
	for _, p := range ports {
		if p.Name != "" {
			skip[strings.ToUpper(p.Name)] = struct{}{}
		}
		skip[strings.ToUpper(p.Port)] = struct{}{}
	}
	for _, s := range PortIDs {
		skip[s] = struct{}{}
	}
	candidates := stringsList
	if target.Name != "" {
		if anchored := stringsAfterFirstAnchor(stringsList, target.Name, target.Port); len(anchored) > 0 {
			candidates = anchored
		}
	} else if target.Port != "" {
		if anchored := stringsAfterFirstAnchor(stringsList, target.Port); len(anchored) > 0 {
			candidates = anchored
		}
	}
	var labels []string
	seen := make(map[string]struct{})
	for _, s := range candidates {
		s = strings.TrimSpace(s)
		key := strings.ToUpper(s)
		if s == "" || len(s) > 20 {
			continue
		}
		if _, ok := skip[key]; ok {
			continue
		}
		if isMetadataString(key) {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		labels = append(labels, s)
		if len(labels) >= max {
			break
		}
	}
	return labels
}

func stringsAfterFirstAnchor(in []string, anchors ...string) []string {
	anchorSet := make(map[string]struct{})
	for _, anchor := range anchors {
		anchor = strings.ToUpper(strings.TrimSpace(anchor))
		if anchor != "" {
			anchorSet[anchor] = struct{}{}
		}
	}
	if len(anchorSet) == 0 {
		return nil
	}
	for i, s := range in {
		if _, ok := anchorSet[strings.ToUpper(strings.TrimSpace(s))]; ok && i+1 < len(in) {
			return in[i+1:]
		}
	}
	return nil
}

func isMetadataString(s string) bool {
	if strings.HasPrefix(s, "TP") || strings.HasPrefix(s, "TH") || strings.HasPrefix(s, "TM") {
		return true
	}
	switch s {
	case "GENAUD", "HEADST", "BP7100", "TP7016", "TP5012", "TP5024", "TP5008", "AKTUALNA":
		return true
	}
	if strings.Contains(s, ".") {
		parts := strings.Split(s, ".")
		if len(parts) == 4 {
			allNum := true
			for _, p := range parts {
				if _, err := strconv.Atoi(p); err != nil {
					allNum = false
					break
				}
			}
			if allNum {
				return true
			}
		}
	}
	return false
}

func UniqueStrings(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		key := strings.ToUpper(s)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, s)
	}
	return out
}

func PayloadSummary(payload []byte) string {
	info, ok := InfoPayload(payload)
	if !ok || len(info) == 0 {
		if len(payload) == 2 && payload[0] == 0x00 {
			return fmt.Sprintf("ctrl=%c", payload[1])
		}
		return fmt.Sprintf("len=%d", len(payload))
	}
	switch info[0] {
	case 0x01:
		bank, name, ok := CurrentMapBank(payload)
		if ok {
			return fmt.Sprintf("map_bank_table current=%d name=%q len=%d", bank, name, len(payload))
		}
		return fmt.Sprintf("map_bank_table len=%d", len(payload))
	case 0x03:
		if len(info) >= 2 {
			return fmt.Sprintf("map_download_request bank=%d", info[1])
		}
		return "map_download_request"
	case 0x04:
		if len(info) >= 5 {
			return fmt.Sprintf("map_size=%d", binary.BigEndian.Uint32(info[1:5]))
		}
		return fmt.Sprintf("map_size len=%d", len(payload))
	case 0x05:
		if len(info) >= 2 {
			return fmt.Sprintf("map_chunk seq=%d bytes=%d", info[1], len(info)-2)
		}
		return fmt.Sprintf("map_chunk len=%d", len(payload))
	case 0x06:
		return "map_end"
	case 0x0E:
		if len(info) >= 2 {
			switch info[1] {
			case 0x00:
				if len(info) >= 3 {
					return fmt.Sprintf("port_status port=%d len=%d", info[2], len(payload))
				}
				return fmt.Sprintf("port_status len=%d", len(payload))
			case 0x01:
				return fmt.Sprintf("identity len=%d", len(payload))
			}
		}
		return fmt.Sprintf("status len=%d", len(payload))
	default:
		return fmt.Sprintf("info_type=0x%02X len=%d", info[0], len(payload))
	}
}

func logTX(debug bool, label string, payload []byte) {
	if debug {
		log.Printf("matrix tcp tx %s payload=[%s] %s", label, payloadHex(payload), PayloadSummary(payload))
	}
}

func logRX(debug bool, label string, payload []byte) {
	if debug {
		log.Printf("matrix tcp rx %s payload=[%s] %s", label, payloadHex(payload), PayloadSummary(payload))
	}
}

func payloadHex(payload []byte) string {
	const max = 96
	if len(payload) <= max {
		return prtp.HexBytes(payload)
	}
	return fmt.Sprintf("%s ... +%dB", prtp.HexBytes(payload[:max]), len(payload)-max)
}
