//go:build (darwin || windows || linux) && cgo

package audio

/*
#include <stdlib.h>
*/
import "C"

import (
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/gen2brain/malgo"
)

const DefaultDeviceID = "default"

type malgoLocal struct {
	playbackEnabled bool
	captureEnabled  bool
	sampleRate      int
	frameSamples    int

	ctx            *malgo.AllocatedContext
	playbackDevice *malgo.Device
	captureDevice  *malgo.Device
	duplexDevice   *malgo.Device
	playbackChans  int
	captureChans   int

	playMu        sync.Mutex
	playbackBuf   []int16
	playbackQueue chan []int16

	captureChan chan []int16

	closeOnce sync.Once
	closed    uint32
}

func Supported() bool { return true }

func NormalizeDeviceID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" || strings.EqualFold(id, DefaultDeviceID) {
		return DefaultDeviceID
	}
	return id
}

func localAudioContextConfig(clientName string) (malgo.ContextConfig, func()) {
	var cfg malgo.ContextConfig
	if runtime.GOOS != "linux" {
		return cfg, func() {}
	}
	cName := C.CString(NormalizeClientName(clientName))
	cfg.Jack.PClientName = (*byte)(unsafe.Pointer(cName))
	cfg.Jack.TryStartServer = 1
	return cfg, func() {
		C.free(unsafe.Pointer(cName))
	}
}

func initLocalAudioContext(clientName string) (*malgo.AllocatedContext, error) {
	cfg, cleanup := localAudioContextConfig(clientName)
	defer cleanup()
	return malgo.InitContext(localAudioBackends(), cfg, nil)
}

func ListDevices() (DeviceSnapshot, error) {
	snap := DeviceSnapshot{
		Supported: true,
		Backend:   BackendName(),
	}
	if localAudioUsesSyntheticDevices() {
		snap.Capture = []DeviceInfo{{
			ID:        DefaultDeviceID,
			Name:      "JACK capture ports",
			Kind:      "capture",
			IsDefault: true,
		}}
		snap.Playback = []DeviceInfo{{
			ID:        DefaultDeviceID,
			Name:      "JACK playback ports",
			Kind:      "playback",
			IsDefault: true,
		}}
		return snap, nil
	}

	ctx, err := initLocalAudioContext(DefaultClientName)
	if err != nil {
		return snap, err
	}
	defer func() {
		_ = ctx.Context.Uninit()
		ctx.Free()
	}()

	capture, err := enumerateLocalAudioDevices(ctx.Context, malgo.Capture, "capture", "System default capture")
	if err != nil {
		return snap, err
	}
	playback, err := enumerateLocalAudioDevices(ctx.Context, malgo.Playback, "playback", "System default playback")
	if err != nil {
		return snap, err
	}
	snap.Capture = capture
	snap.Playback = playback
	return snap, nil
}

func enumerateLocalAudioDevices(ctx malgo.Context, kind malgo.DeviceType, labelKind, defaultName string) ([]DeviceInfo, error) {
	out := []DeviceInfo{{
		ID:        DefaultDeviceID,
		Name:      defaultName,
		Kind:      labelKind,
		IsDefault: true,
	}}
	infos, err := ctx.Devices(kind)
	if err != nil {
		return out, err
	}
	for i := range infos {
		name := strings.TrimSpace(infos[i].Name())
		if name == "" {
			name = infos[i].ID.String()
		}
		out = append(out, DeviceInfo{
			ID:        infos[i].ID.String(),
			Name:      name,
			Kind:      labelKind,
			IsDefault: infos[i].IsDefault != 0,
		})
	}
	return out, nil
}

func NewLocal(playback, capture bool, sampleRate, frameSamples int) (Local, error) {
	return NewLocalWithDevices(playback, capture, sampleRate, frameSamples, DefaultDeviceID, DefaultDeviceID)
}

func NewLocalWithDevices(playback, capture bool, sampleRate, frameSamples int, playbackID, captureID string) (Local, error) {
	return NewLocalWithOptions(LocalOptions{
		Playback:     playback,
		Capture:      capture,
		SampleRate:   sampleRate,
		FrameSamples: frameSamples,
		PlaybackID:   playbackID,
		CaptureID:    captureID,
		ClientName:   DefaultClientName,
	})
}

func NewLocalWithOptions(opts LocalOptions) (Local, error) {
	if !opts.Playback && !opts.Capture {
		return nil, nil
	}
	sampleRate := opts.SampleRate
	if sampleRate <= 0 {
		sampleRate = 16000
	}
	frameSamples := opts.FrameSamples
	if frameSamples <= 0 {
		frameSamples = 256
	}
	playbackID := NormalizeDeviceID(opts.PlaybackID)
	captureID := NormalizeDeviceID(opts.CaptureID)

	ctx, err := initLocalAudioContext(opts.ClientName)
	if err != nil {
		return nil, err
	}

	la := &malgoLocal{
		playbackEnabled: opts.Playback,
		captureEnabled:  opts.Capture,
		sampleRate:      sampleRate,
		frameSamples:    frameSamples,
		ctx:             ctx,
		playbackChans:   int(localAudioClientChannels()),
		captureChans:    int(localAudioClientChannels()),
		playbackQueue:   make(chan []int16, LocalAudioQueueFrames),
	}
	if la.playbackChans <= 0 {
		la.playbackChans = 1
	}
	if la.captureChans <= 0 {
		la.captureChans = 1
	}
	if opts.Capture {
		la.captureChan = make(chan []int16, LocalAudioQueueFrames)
	}

	if (opts.Playback || opts.Capture) && localAudioPrefersDuplex() {
		if err := la.initDuplexDevice(playbackID, captureID); err != nil {
			_ = la.Close()
			return nil, err
		}
		return la, nil
	}

	if opts.Playback {
		if err := la.initPlaybackDevice(playbackID); err != nil {
			_ = la.Close()
			return nil, err
		}
	}
	if opts.Capture {
		if err := la.initCaptureDevice(captureID); err != nil {
			_ = la.Close()
			return nil, err
		}
	}
	return la, nil
}

func (la *malgoLocal) initPlaybackDevice(deviceID string) error {
	devicePtr, cleanup, err := la.resolveDevicePointer(malgo.Playback, deviceID)
	if err != nil {
		return err
	}
	defer cleanup()

	cfg := malgo.DefaultDeviceConfig(malgo.Playback)
	cfg.Playback.DeviceID = devicePtr
	cfg.Playback.Format = malgo.FormatS16
	cfg.Playback.Channels = localAudioClientChannels()
	cfg.SampleRate = uint32(la.sampleRate)
	cfg.PeriodSizeInFrames = uint32(la.frameSamples)
	cfg.Periods = 2
	dev, err := malgo.InitDevice(la.ctx.Context, cfg, malgo.DeviceCallbacks{
		Data: la.playbackCallback,
	})
	if err != nil {
		return err
	}
	if err := dev.Start(); err != nil {
		dev.Uninit()
		return err
	}
	la.playbackDevice = dev
	la.playbackChans = int(dev.PlaybackChannels())
	if la.playbackChans <= 0 {
		la.playbackChans = int(localAudioClientChannels())
	}
	return nil
}

func (la *malgoLocal) initCaptureDevice(deviceID string) error {
	devicePtr, cleanup, err := la.resolveDevicePointer(malgo.Capture, deviceID)
	if err != nil {
		return err
	}
	defer cleanup()

	cfg := malgo.DefaultDeviceConfig(malgo.Capture)
	cfg.Capture.DeviceID = devicePtr
	cfg.Capture.Format = malgo.FormatS16
	cfg.Capture.Channels = localAudioClientChannels()
	cfg.SampleRate = uint32(la.sampleRate)
	cfg.PeriodSizeInFrames = uint32(la.frameSamples)
	cfg.Periods = 2
	dev, err := malgo.InitDevice(la.ctx.Context, cfg, malgo.DeviceCallbacks{
		Data: la.captureCallback,
	})
	if err != nil {
		return err
	}
	if err := dev.Start(); err != nil {
		dev.Uninit()
		return err
	}
	la.captureDevice = dev
	la.captureChans = int(dev.CaptureChannels())
	if la.captureChans <= 0 {
		la.captureChans = int(localAudioClientChannels())
	}
	return nil
}

func (la *malgoLocal) initDuplexDevice(playbackID, captureID string) error {
	playbackPtr, playbackCleanup, err := la.resolveDevicePointer(malgo.Playback, playbackID)
	if err != nil {
		return err
	}
	defer playbackCleanup()
	capturePtr, captureCleanup, err := la.resolveDevicePointer(malgo.Capture, captureID)
	if err != nil {
		return err
	}
	defer captureCleanup()

	cfg := malgo.DefaultDeviceConfig(malgo.Duplex)
	cfg.Playback.DeviceID = playbackPtr
	cfg.Playback.Format = malgo.FormatS16
	cfg.Playback.Channels = localAudioClientChannels()
	cfg.Capture.DeviceID = capturePtr
	cfg.Capture.Format = malgo.FormatS16
	cfg.Capture.Channels = localAudioClientChannels()
	cfg.SampleRate = uint32(la.sampleRate)
	cfg.PeriodSizeInFrames = uint32(la.frameSamples)
	cfg.Periods = 2
	dev, err := malgo.InitDevice(la.ctx.Context, cfg, malgo.DeviceCallbacks{
		Data: la.duplexCallback,
	})
	if err != nil {
		return err
	}
	if err := dev.Start(); err != nil {
		dev.Uninit()
		return err
	}
	la.duplexDevice = dev
	la.playbackChans = int(dev.PlaybackChannels())
	la.captureChans = int(dev.CaptureChannels())
	if la.playbackChans <= 0 {
		la.playbackChans = int(localAudioClientChannels())
	}
	if la.captureChans <= 0 {
		la.captureChans = int(localAudioClientChannels())
	}
	return nil
}

func (la *malgoLocal) resolveDevicePointer(kind malgo.DeviceType, deviceID string) (unsafe.Pointer, func(), error) {
	deviceID = NormalizeDeviceID(deviceID)
	if deviceID == DefaultDeviceID {
		return nil, func() {}, nil
	}
	if localAudioUsesSyntheticDevices() {
		return nil, func() {}, fmt.Errorf("%s backend only supports the default JACK device", BackendName())
	}
	infos, err := la.ctx.Context.Devices(kind)
	if err != nil {
		return nil, func() {}, err
	}
	for i := range infos {
		if infos[i].ID.String() != deviceID {
			continue
		}
		ptr := infos[i].ID.Pointer()
		return ptr, func() {
			C.free(ptr)
		}, nil
	}
	return nil, func() {}, fmt.Errorf("audio device %q not found", deviceID)
}

func (la *malgoLocal) duplexCallback(outputSamples, inputSamples []byte, frameCount uint32) {
	la.playbackCallback(outputSamples, nil, frameCount)
	la.captureCallback(nil, inputSamples, frameCount)
}

func (la *malgoLocal) playbackCallback(outputSamples, _ []byte, frameCount uint32) {
	if atomic.LoadUint32(&la.closed) == 1 {
		return
	}
	out := bytesToInt16(outputSamples)
	channels := la.playbackChans
	if channels <= 0 {
		channels = 1
	}
	samplesNeeded := len(out) / channels

	la.playMu.Lock()
	defer la.playMu.Unlock()

	for samplesNeeded > len(la.playbackBuf) {
		var (
			chunk []int16
			ok    bool
		)
		select {
		case chunk, ok = <-la.playbackQueue:
		default:
			ok = false
		}
		if !ok || len(chunk) == 0 {
			break
		}
		la.playbackBuf = append(la.playbackBuf, chunk...)
		if maxBuffered := la.frameSamples * LocalAudioBufferedFrames; maxBuffered > 0 && len(la.playbackBuf) > maxBuffered {
			la.playbackBuf = la.playbackBuf[len(la.playbackBuf)-maxBuffered:]
		}
		if samplesNeeded <= len(la.playbackBuf) {
			break
		}
	}

	copied := 0
	for copied < samplesNeeded && copied < len(la.playbackBuf) {
		sample := la.playbackBuf[copied]
		base := copied * channels
		for ch := 0; ch < channels && base+ch < len(out); ch++ {
			out[base+ch] = sample
		}
		copied++
	}
	if copied > 0 {
		la.playbackBuf = la.playbackBuf[copied:]
	}
	for frame := copied; frame < samplesNeeded; frame++ {
		base := frame * channels
		for ch := 0; ch < channels && base+ch < len(out); ch++ {
			out[base+ch] = 0
		}
	}
}

func (la *malgoLocal) captureCallback(_, inputSamples []byte, frameCount uint32) {
	if !la.captureEnabled || la.captureChan == nil || atomic.LoadUint32(&la.closed) == 1 {
		return
	}
	in := bytesToInt16(inputSamples)
	if len(in) == 0 {
		return
	}
	frame := monoFromInterleaved(in, la.captureChans)
	select {
	case la.captureChan <- frame:
	default:
		for {
			select {
			case <-la.captureChan:
				continue
			default:
			}
			break
		}
		select {
		case la.captureChan <- frame:
		default:
		}
	}
}

func monoFromInterleaved(in []int16, channels int) []int16 {
	if channels <= 1 {
		frame := make([]int16, len(in))
		copy(frame, in)
		return frame
	}
	frames := len(in) / channels
	out := make([]int16, frames)
	for i := 0; i < frames; i++ {
		base := i * channels
		sum := 0
		for ch := 0; ch < channels && base+ch < len(in); ch++ {
			sum += int(in[base+ch])
		}
		out[i] = int16(sum / channels)
	}
	return out
}

func (la *malgoLocal) Play(samples []int16) {
	if !la.playbackEnabled || len(samples) == 0 || atomic.LoadUint32(&la.closed) == 1 {
		return
	}
	chunk := make([]int16, len(samples))
	copy(chunk, samples)
	select {
	case la.playbackQueue <- chunk:
	default:
		for {
			select {
			case <-la.playbackQueue:
				continue
			default:
			}
			break
		}
		select {
		case la.playbackQueue <- chunk:
		default:
		}
	}
}

func (la *malgoLocal) RecordChan() <-chan []int16 {
	return la.captureChan
}

func (la *malgoLocal) Close() error {
	var err error
	la.closeOnce.Do(func() {
		atomic.StoreUint32(&la.closed, 1)
		if la.playbackDevice != nil {
			_ = la.playbackDevice.Stop()
			la.playbackDevice.Uninit()
		}
		if la.captureDevice != nil {
			_ = la.captureDevice.Stop()
			la.captureDevice.Uninit()
		}
		if la.duplexDevice != nil {
			_ = la.duplexDevice.Stop()
			la.duplexDevice.Uninit()
		}
		if la.ctx != nil {
			_ = la.ctx.Context.Uninit()
			la.ctx.Free()
		}
		if la.captureChan != nil {
			close(la.captureChan)
		}
	})
	return err
}

func (la *malgoLocal) SampleRate() int       { return la.sampleRate }
func (la *malgoLocal) FrameSamples() int     { return la.frameSamples }
func (la *malgoLocal) PlaybackEnabled() bool { return la.playbackEnabled }
func (la *malgoLocal) CaptureEnabled() bool  { return la.captureEnabled }

func bytesToInt16(b []byte) []int16 {
	if len(b) < 2 {
		return nil
	}
	count := len(b) / 2
	return unsafe.Slice((*int16)(unsafe.Pointer(&b[0])), count)
}
