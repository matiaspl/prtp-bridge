package audio

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	LocalAudioQueueFrames    = 4
	LocalAudioBufferedFrames = 2
	DefaultClientName        = "prtp-bridge"
	maxClientNameLen         = 63
)

var clientNameUnsafe = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

type Local interface {
	Play([]int16)
	RecordChan() <-chan []int16
	Close() error
	SampleRate() int
	FrameSamples() int
	PlaybackEnabled() bool
	CaptureEnabled() bool
}

type DeviceInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	IsDefault bool   `json:"is_default"`
}

type DeviceSnapshot struct {
	Supported bool         `json:"supported"`
	Backend   string       `json:"backend"`
	Capture   []DeviceInfo `json:"capture"`
	Playback  []DeviceInfo `json:"playback"`
}

type LocalOptions struct {
	Playback     bool
	Capture      bool
	SampleRate   int
	FrameSamples int
	PlaybackID   string
	CaptureID    string
	ClientName   string
}

func NormalizeClientName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = DefaultClientName
	}
	name = clientNameUnsafe.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-_.")
	if name == "" {
		name = DefaultClientName
	}
	if len(name) > maxClientNameLen {
		name = strings.TrimRight(name[:maxClientNameLen], "-_.")
		if name == "" {
			name = DefaultClientName
		}
	}
	return name
}

type Source interface {
	Next([]int16)
}

type SilenceSource struct{}

func (SilenceSource) Next(out []int16) {
	for i := range out {
		out[i] = 0
	}
}

type ToneSource struct {
	sampleRate int
	channels   int
	frequency  float64
	phase      float64
}

func NewToneSource(sampleRate, channels int, frequency float64) *ToneSource {
	if sampleRate <= 0 {
		sampleRate = 8000
	}
	if channels <= 0 {
		channels = 1
	}
	if frequency <= 0 {
		frequency = 1000
	}
	return &ToneSource{sampleRate: sampleRate, channels: channels, frequency: frequency}
}

func (s *ToneSource) Next(out []int16) {
	for i := range out {
		v := math.Sin(2 * math.Pi * s.phase)
		out[i] = int16(v * 12000)
		s.phase += s.frequency / float64(s.sampleRate)
		if s.phase >= 1 {
			s.phase -= math.Floor(s.phase)
		}
	}
}

var DefaultFIRCoeffs = []float64{
	-8.23974609375e-4,
	9.918212890625e-4,
	0.002761840820313,
	0.002059936523438,
	-0.003097534179688,
	-0.008987426757813,
	-0.00634765625,
	0.008819580078125,
	0.02388000488281,
	0.0159912109375,
	-0.02149963378906,
	-0.05804443359375,
	-0.04034423828125,
	0.05996704101563,
	0.2075042724609,
	0.3171691894531,
	0.3171691894531,
	0.2075042724609,
	0.05996704101563,
	-0.04034423828125,
	-0.05804443359375,
	-0.02149963378906,
	0.0159912109375,
	0.02388000488281,
	0.008819580078125,
	-0.00634765625,
	-0.008987426757813,
	-0.003097534179688,
	0.002059936523438,
	0.002761840820313,
	9.918212890625e-4,
	-8.23974609375e-4,
}

type FIRFilter struct {
	coeffs []float64
	delay  []float64
	idx    int
}

func NewFIRFilter(coeffs []float64) *FIRFilter {
	return &FIRFilter{
		coeffs: append([]float64(nil), coeffs...),
		delay:  make([]float64, len(coeffs)),
	}
}

func NewTXAntiAliasFIR(enabled bool, inRate, outRate int) *FIRFilter {
	if !enabled || inRate <= 0 || outRate <= 0 || inRate <= outRate {
		return nil
	}
	return NewFIRFilter(DefaultFIRCoeffs)
}

func (f *FIRFilter) Process(samples []int16) {
	if f == nil {
		return
	}
	for i, s := range samples {
		f.delay[f.idx] = float64(s)
		sum := 0.0
		di := f.idx
		for j := 0; j < len(f.coeffs); j++ {
			sum += f.coeffs[j] * f.delay[di]
			di--
			if di < 0 {
				di = len(f.delay) - 1
			}
		}
		samples[i] = floatToInt16(sum)
		f.idx++
		if f.idx == len(f.delay) {
			f.idx = 0
		}
	}
}

func floatToInt16(v float64) int16 {
	if v > 32767 {
		v = 32767
	} else if v < -32768 {
		v = -32768
	}
	return int16(math.Round(v))
}

type Int16StreamResampler struct {
	inRate  int
	outRate int
	buf     []int16
	pos     float64
}

func NewInt16StreamResampler(inRate, outRate int) *Int16StreamResampler {
	r := &Int16StreamResampler{}
	r.Reset(inRate, outRate)
	return r
}

func (r *Int16StreamResampler) Reset(inRate, outRate int) {
	if inRate <= 0 {
		inRate = outRate
	}
	if outRate <= 0 {
		outRate = inRate
	}
	r.inRate = inRate
	r.outRate = outRate
	r.buf = nil
	r.pos = 0
}

func (r *Int16StreamResampler) Process(in []int16) []int16 {
	if len(in) == 0 {
		return nil
	}
	if r.inRate == r.outRate {
		out := make([]int16, len(in))
		copy(out, in)
		return out
	}
	r.buf = append(r.buf, in...)
	step := float64(r.inRate) / float64(r.outRate)
	out := make([]int16, 0, int(float64(len(in))*float64(r.outRate)/float64(r.inRate))+2)
	for r.pos+1 < float64(len(r.buf)) {
		j := int(math.Floor(r.pos))
		frac := r.pos - float64(j)
		s0 := float64(r.buf[j])
		s1 := float64(r.buf[j+1])
		v := s0*(1-frac) + s1*frac
		if v > 32767 {
			v = 32767
		}
		if v < -32768 {
			v = -32768
		}
		out = append(out, int16(v))
		r.pos += step
	}
	drop := int(math.Floor(r.pos))
	if drop > 0 {
		if drop > len(r.buf)-1 {
			drop = len(r.buf) - 1
		}
		copy(r.buf, r.buf[drop:])
		r.buf = r.buf[:len(r.buf)-drop]
		r.pos -= float64(drop)
	}
	return out
}

func ResampleInt16(in []int16, inRate, outRate int) []int16 {
	if inRate == outRate || len(in) == 0 {
		out := make([]int16, len(in))
		copy(out, in)
		return out
	}
	ratio := float64(outRate) / float64(inRate)
	outLen := int(math.Round(float64(len(in)) * ratio))
	if outLen <= 0 {
		outLen = 1
	}
	out := make([]int16, outLen)
	for i := 0; i < outLen; i++ {
		srcPos := float64(i) / ratio
		j := int(math.Floor(srcPos))
		frac := srcPos - float64(j)
		s0 := float64(in[min(j, len(in)-1)])
		s1 := float64(in[min(j+1, len(in)-1)])
		v := s0*(1-frac) + s1*frac
		if v > 32767 {
			v = 32767
		}
		if v < -32768 {
			v = -32768
		}
		out[i] = int16(v)
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

type DumpSink struct {
	path   string
	file   *os.File
	writer *bufio.Writer
	mu     sync.Mutex
}

func OpenDumpSink(spec, name, ext string) (*DumpSink, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	path, isPipe, err := ResolveDumpPath(spec, name, ext)
	if err != nil {
		return nil, err
	}
	var file *os.File
	if isPipe {
		file, err = os.OpenFile(path, os.O_WRONLY, 0)
	} else {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return nil, err
		}
		file, err = os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	}
	if err != nil {
		return nil, err
	}
	return &DumpSink{path: path, file: file, writer: bufio.NewWriterSize(file, 64*1024)}, nil
}

func ResolveDumpPath(spec, name, ext string) (path string, isPipe bool, err error) {
	if st, statErr := os.Stat(spec); statErr == nil {
		if st.Mode()&os.ModeNamedPipe != 0 {
			return spec, true, nil
		}
		if st.IsDir() {
			ts := time.Now().Format("20060102-150405.000")
			return filepath.Join(spec, fmt.Sprintf("%s-%s.%s", ts, name, ext)), false, nil
		}
		return spec, false, nil
	}
	ts := time.Now().Format("20060102-150405.000")
	path = strings.ReplaceAll(spec, "{ts}", ts)
	path = strings.ReplaceAll(path, "{name}", name)
	path = strings.ReplaceAll(path, "{ext}", ext)
	if strings.HasSuffix(path, string(os.PathSeparator)) {
		return filepath.Join(path, fmt.Sprintf("%s-%s.%s", ts, name, ext)), false, nil
	}
	if filepath.Ext(path) == "" && !strings.Contains(spec, "{") {
		return filepath.Join(path, fmt.Sprintf("%s-%s.%s", ts, name, ext)), false, nil
	}
	return path, false, nil
}

func (d *DumpSink) Path() string {
	if d == nil {
		return ""
	}
	return d.path
}

func (d *DumpSink) Write(b []byte) {
	if d == nil || len(b) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, _ = d.writer.Write(b)
}

func (d *DumpSink) WriteInt16(samples []int16) {
	if d == nil || len(samples) == 0 {
		return
	}
	buf := make([]byte, len(samples)*2)
	for i, v := range samples {
		binary.LittleEndian.PutUint16(buf[2*i:], uint16(v))
	}
	d.Write(buf)
}

func (d *DumpSink) Close() error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_ = d.writer.Flush()
	return d.file.Close()
}
