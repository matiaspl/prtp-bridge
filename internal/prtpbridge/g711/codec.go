package g711

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"

	baseg711 "prtp-bridge/internal/g711"
)

type Codec struct {
	mode      string
	decodeTbl []int16
	encodeLUT []byte
	txMap     []byte
}

type TableFile struct {
	Decode []int16 `json:"decode"`
}

type TXMapFile struct {
	Type        string       `json:"type,omitempty"`
	Description string       `json:"description,omitempty"`
	Source      string       `json:"source,omitempty"`
	G711Mode    string       `json:"g711_mode,omitempty"`
	TXMap       []int        `json:"tx_map,omitempty"`
	Map         []int        `json:"map,omitempty"`
	Predicted   TXMapQuality `json:"predicted,omitempty"`
	Entries     []TXMapEntry `json:"entries,omitempty"`
}

type TXMapEntry struct {
	DesiredCode   int     `json:"desired_code"`
	DesiredPCM    int     `json:"desired_pcm"`
	TXCode        int     `json:"tx_code"`
	PredictedPCM  float64 `json:"predicted_pcm"`
	PredictedCode int     `json:"predicted_code"`
	AbsError      float64 `json:"abs_error"`
	Samples       int     `json:"samples"`
}

type TXMapQuality struct {
	CodesMapped         int     `json:"codes_mapped"`
	MeanAbsError        float64 `json:"mean_abs_error"`
	MaxAbsError         float64 `json:"max_abs_error"`
	ExactPCMCodes       int     `json:"exact_pcm_codes"`
	UnmappedCodes       int     `json:"unmapped_codes"`
	UniqueTXCodes       int     `json:"unique_tx_codes"`
	ManyToOneCollisions int     `json:"many_to_one_collisions"`
}

func New(mode, tablePath string) (*Codec, string, error) {
	rawMode := mode
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = "custom"
	}
	if mode != "custom" && mode != "alaw" {
		return nil, "", fmt.Errorf("invalid g711 mode %q (expected custom|alaw)", rawMode)
	}

	decode := baseg711.DefaultDecodeTable()
	source := "built-in"
	if mode == "custom" && strings.TrimSpace(tablePath) != "" {
		b, err := os.ReadFile(tablePath)
		if err != nil {
			return nil, "", fmt.Errorf("read g711 table: %w", err)
		}
		var tf TableFile
		if err := json.Unmarshal(b, &tf); err != nil {
			return nil, "", fmt.Errorf("parse g711 table: %w", err)
		}
		decode = tf.Decode
		source = tablePath
	}
	if len(decode) != 256 {
		return nil, "", fmt.Errorf("decode table must have 256 entries")
	}

	codec := &Codec{mode: mode, decodeTbl: make([]int16, 256)}
	copy(codec.decodeTbl, decode)
	if mode == "custom" {
		codec.encodeLUT = MakeEncodeLUT(codec.decodeTbl)
	}
	return codec, source, nil
}

func (c *Codec) Mode() string {
	if c == nil {
		return ""
	}
	return c.mode
}

func (c *Codec) DecodeInto(out []int16, buf []byte) []int16 {
	if c == nil {
		return nil
	}
	if cap(out) < len(buf) {
		out = make([]int16, len(buf))
	} else {
		out = out[:len(buf)]
	}
	for i, b := range buf {
		if c.mode == "alaw" {
			decoded := AlawDecode(b)
			scaled := int(decoded) * 8
			if scaled > 32767 {
				scaled = 32767
			} else if scaled < -32768 {
				scaled = -32768
			}
			out[i] = int16(scaled)
			continue
		}
		signBit := ((int(b) ^ 0xFF) << 8) & 0x8000
		index := int(b) ^ 0x55
		tableVal := int(c.decodeTbl[index])
		output := (tableVal << 3) | 0x3 | signBit
		if output >= 32768 {
			output -= 65536
		}
		out[i] = int16(output)
	}
	return out
}

func (c *Codec) Decode(buf []byte) []int16 {
	return c.DecodeInto(nil, buf)
}

func (c *Codec) EncodeInto(out []byte, pcm []int16) []byte {
	return c.encodeIntoMapped(out, pcm, true)
}

func (c *Codec) EncodeIntoUnmapped(out []byte, pcm []int16) []byte {
	return c.encodeIntoMapped(out, pcm, false)
}

func (c *Codec) encodeIntoMapped(out []byte, pcm []int16, applyTXMap bool) []byte {
	if c == nil {
		return nil
	}
	if cap(out) < len(pcm) {
		out = make([]byte, len(pcm))
	} else {
		out = out[:len(pcm)]
	}
	for i, s := range pcm {
		code := c.EncodeSampleCode(s)
		if applyTXMap && c.txMap != nil {
			code = c.txMap[int(code)]
		}
		out[i] = code
	}
	return out
}

func (c *Codec) Encode(pcm []int16) []byte {
	return c.EncodeInto(nil, pcm)
}

func (c *Codec) EncodeSampleCode(s int16) byte {
	if c.mode == "alaw" {
		return AlawEncode(s)
	}
	return c.encodeLUT[int(s)+32768]
}

func (c *Codec) SetTXMap(txMap []byte) {
	if c == nil || len(txMap) != 256 {
		return
	}
	c.txMap = append([]byte(nil), txMap...)
}

func LoadTXMap(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read g711 tx map: %w", err)
	}
	var raw []int
	if err := json.Unmarshal(b, &raw); err == nil && len(raw) > 0 {
		return ParseTXMapInts(raw)
	}
	var mf TXMapFile
	if err := json.Unmarshal(b, &mf); err != nil {
		return nil, fmt.Errorf("parse g711 tx map: %w", err)
	}
	values := mf.TXMap
	if len(values) == 0 {
		values = mf.Map
	}
	return ParseTXMapInts(values)
}

func ParseTXMapInts(values []int) ([]byte, error) {
	if len(values) != 256 {
		return nil, fmt.Errorf("g711 tx map must contain 256 entries, got %d", len(values))
	}
	out := make([]byte, 256)
	for i, v := range values {
		if v < 0 || v > 255 {
			return nil, fmt.Errorf("g711 tx map entry %d is outside byte range: %d", i, v)
		}
		out[i] = byte(v)
	}
	return out, nil
}

func TXMapToInts(txMap []byte) []int {
	out := make([]int, 256)
	for i := range out {
		if i < len(txMap) {
			out[i] = int(txMap[i])
		} else {
			out[i] = i
		}
	}
	return out
}

func MakeEncodeLUT(decode []int16) []byte {
	compressor := make([]byte, 4096)

	for pcmShifted := 0; pcmShifted < 4096; pcmShifted++ {
		targetPCM := (pcmShifted << 4) | 0x08
		if targetPCM >= 0x8000 {
			targetPCM -= 0x10000
		}

		bestRawCode := 0
		bestDist := int(^uint(0) >> 1)

		for rawCode := 0; rawCode < 256; rawCode++ {
			wireCode := rawCode ^ 0x55
			signBit := ((wireCode ^ 0xFF) << 8) & 0x8000
			index := wireCode ^ 0x55
			tableVal := int(decode[index])
			decoded := (tableVal << 3) | 0x3 | signBit
			if decoded >= 32768 {
				decoded -= 65536
			}

			dist := decoded - targetPCM
			if dist < 0 {
				dist = -dist
			}
			if dist < bestDist {
				bestDist = dist
				bestRawCode = rawCode
			}
		}

		compressor[pcmShifted] = byte(bestRawCode)
	}

	lut := make([]byte, 65536)
	for i := 0; i < 65536; i++ {
		s := int16(i - 32768)
		pcmUnsigned := int(s)
		if pcmUnsigned < 0 {
			pcmUnsigned += 0x10000
		}
		index := pcmUnsigned >> 4
		if index >= 4096 {
			index = 4095
		}
		lut[i] = compressor[index] ^ 0x55
	}
	return lut
}

func AlawDecode(code byte) int16 {
	a := code ^ 0x55
	t := int(a&0x0F) << 4
	seg := int((a & 0x70) >> 4)
	switch seg {
	case 0:
		t += 8
	case 1:
		t += 0x108
	default:
		t += 0x108
		t <<= (seg - 1)
	}
	if (a & 0x80) != 0 {
		return int16(t)
	}
	return int16(-t)
}

func AlawEncode(sample int16) byte {
	const (
		ALAWMax = 0x7FF
		bias    = 0x55
	)

	var sign byte
	if sample < 0 {
		sample = -sample
		sign = 0x80
	}
	if sample > ALAWMax {
		sample = ALAWMax
	}

	seg := 0
	tmp := int(sample)
	for tmp > 0x1F {
		tmp >>= 1
		seg++
	}

	var aval byte
	if seg == 0 {
		aval = byte((int(sample) >> 1) & 0x0F)
	} else {
		aval = byte((seg << 4) | ((int(sample) >> uint(seg+2)) & 0x0F))
	}
	return aval ^ (sign ^ bias)
}

func RoundTripCorrelation(codec *Codec, src []int16) (corr, rmsErr float64) {
	got := codec.Decode(codec.Encode(src))
	var meanSrc, meanGot float64
	for i := range src {
		meanSrc += float64(src[i])
		meanGot += float64(got[i])
	}
	meanSrc /= float64(len(src))
	meanGot /= float64(len(got))

	var dot, src2, got2, err2 float64
	for i := range src {
		s := float64(src[i]) - meanSrc
		g := float64(got[i]) - meanGot
		dot += s * g
		src2 += s * s
		got2 += g * g
		err := float64(got[i] - src[i])
		err2 += err * err
	}
	return dot / math.Sqrt(src2*got2), math.Sqrt(err2 / float64(len(src)))
}
