package gateway

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"prtp-bridge/internal/prtpbridge/audio"
	"prtp-bridge/internal/prtpbridge/config"
	"prtp-bridge/internal/prtpbridge/g711"
	"prtp-bridge/internal/prtpbridge/matrix"
	"prtp-bridge/internal/prtpbridge/matrixhelper"
	"prtp-bridge/internal/prtpbridge/prtp"
	"prtp-bridge/internal/prtpbridge/webui"

	"github.com/gorilla/websocket"
)

const (
	txQueueFrames        = 4
	rxPlaybackFrames     = 8
	wsTextQueueMessages  = 512
	wsAudioQueueFrames   = 4
	serverFrameSamples   = 256
	helperRequestTimeout = 20 * time.Second
	httpShutdownTimeout  = 2 * time.Second
)

type prtpControlPriority int

const (
	prtpControlHigh prtpControlPriority = iota
	prtpControlNormal
)

func audioClientName(cfg config.Config) string {
	if strings.TrimSpace(cfg.Audio.ClientName) != "" {
		return audio.NormalizeClientName(cfg.Audio.ClientName)
	}
	token := strings.TrimSpace(cfg.Instance)
	if token == "" {
		token = strings.TrimSpace(cfg.MatrixPort)
	}
	if token == "" {
		return audio.DefaultClientName
	}
	return audio.NormalizeClientName(audio.DefaultClientName + "-" + token)
}

type prtpControlQueue struct {
	active []byte
	high   [][]byte
	normal [][]byte
}

func (q *prtpControlQueue) enqueue(frame []byte, priority prtpControlPriority) {
	if len(frame) == 0 {
		return
	}
	queued := append([]byte(nil), frame...)
	if priority == prtpControlNormal {
		q.normal = append(q.normal, queued)
		return
	}
	q.high = append(q.high, queued)
}

func (q *prtpControlQueue) pendingBytes() int {
	n := len(q.active)
	for _, frame := range q.high {
		n += len(frame)
	}
	for _, frame := range q.normal {
		n += len(frame)
	}
	return n
}

func (q *prtpControlQueue) pop(max int) []byte {
	if max <= 0 {
		return nil
	}
	if len(q.active) == 0 {
		switch {
		case len(q.high) > 0:
			q.active = q.high[0]
			q.high = q.high[1:]
		case len(q.normal) > 0:
			q.active = q.normal[0]
			q.normal = q.normal[1:]
		default:
			return nil
		}
	}
	if max > len(q.active) {
		max = len(q.active)
	}
	out := append([]byte(nil), q.active[:max]...)
	q.active = q.active[max:]
	return out
}

type rxReorderStats struct {
	gapEvents uint64
	reordered uint64
	missing   uint64
	stale     uint64
}

type rxBufferedPacket struct {
	addr   net.Addr
	packet []byte
}

type rxFragmentStats struct {
	recovered uint64
	dropped   uint64
}

type rxFragmentAssembler struct {
	addr      string
	startedAt time.Time
	buf       []byte
}

func (a *rxFragmentAssembler) push(now time.Time, addr net.Addr, packet []byte) ([]rxBufferedPacket, rxFragmentStats) {
	var stats rxFragmentStats
	if len(packet) == prtp.AudioFrameSize {
		if len(a.buf) > 0 {
			stats.dropped++
			a.reset()
		}
		return []rxBufferedPacket{{addr: addr, packet: append([]byte(nil), packet...)}}, stats
	}
	if len(packet) == 0 || len(packet) > prtp.AudioFrameSize {
		if len(a.buf) > 0 {
			stats.dropped++
			a.reset()
		}
		stats.dropped++
		return nil, stats
	}

	key := addr.String()
	if len(a.buf) > 0 && (a.addr != key || now.Sub(a.startedAt) > 100*time.Millisecond || len(a.buf)+len(packet) > prtp.AudioFrameSize) {
		stats.dropped++
		a.reset()
	}
	if len(a.buf) == 0 {
		a.addr = key
		a.startedAt = now
	}
	a.buf = append(a.buf, packet...)
	if len(a.buf) < prtp.AudioFrameSize {
		return nil, stats
	}

	assembled := append([]byte(nil), a.buf...)
	a.reset()
	if !prtp.IsAudioPacket(assembled) {
		stats.dropped++
		return nil, stats
	}
	stats.recovered++
	return []rxBufferedPacket{{addr: addr, packet: assembled}}, stats
}

func (a *rxFragmentAssembler) reset() {
	a.addr = ""
	a.startedAt = time.Time{}
	a.buf = a.buf[:0]
}

type rxReorderBuffer struct {
	initialized bool
	expected    byte
	pending     map[byte]rxBufferedPacket
	waitingFrom time.Time
	wait        time.Duration
}

func newRXReorderBuffer(wait time.Duration) *rxReorderBuffer {
	return &rxReorderBuffer{
		pending: make(map[byte]rxBufferedPacket),
		wait:    wait,
	}
}

func (b *rxReorderBuffer) hasPending() bool {
	return len(b.pending) > 0
}

func (b *rxReorderBuffer) push(now time.Time, addr net.Addr, packet []byte) ([]rxBufferedPacket, rxReorderStats) {
	var stats rxReorderStats
	if !prtp.IsAudioPacket(packet) {
		return []rxBufferedPacket{{addr: addr, packet: append([]byte(nil), packet...)}}, stats
	}
	queued := rxBufferedPacket{addr: addr, packet: append([]byte(nil), packet...)}
	seq := packet[1]
	if !b.initialized {
		b.initialized = true
		b.expected = seq + 1
		return []rxBufferedPacket{queued}, stats
	}
	if seq == b.expected {
		ready := []rxBufferedPacket{queued}
		b.expected++
		ready = append(ready, b.drainReady(now, &stats, true)...)
		return ready, stats
	}
	delta := seqDistance(b.expected, seq)
	if delta > 0 && delta < 128 {
		if _, exists := b.pending[seq]; exists {
			stats.stale++
			return nil, stats
		}
		b.pending[seq] = queued
		stats.gapEvents++
		if b.waitingFrom.IsZero() {
			b.waitingFrom = now
		}
		return b.flushExpired(now, &stats), stats
	}
	stats.stale++
	return nil, stats
}

func (b *rxReorderBuffer) flushExpired(now time.Time, stats *rxReorderStats) []rxBufferedPacket {
	if !b.initialized || len(b.pending) == 0 || b.waitingFrom.IsZero() || now.Sub(b.waitingFrom) < b.wait {
		return nil
	}
	for skipped := 0; skipped < 128; skipped++ {
		if _, ok := b.pending[b.expected]; ok {
			return b.drainReady(now, stats, false)
		}
		stats.missing++
		b.expected++
	}
	stats.stale += uint64(len(b.pending))
	b.pending = make(map[byte]rxBufferedPacket)
	b.waitingFrom = time.Time{}
	return nil
}

func (b *rxReorderBuffer) drainReady(now time.Time, stats *rxReorderStats, countReordered bool) []rxBufferedPacket {
	var ready []rxBufferedPacket
	for {
		packet, ok := b.pending[b.expected]
		if !ok {
			break
		}
		delete(b.pending, b.expected)
		ready = append(ready, packet)
		if countReordered {
			stats.reordered++
		}
		b.expected++
	}
	if len(b.pending) == 0 {
		b.waitingFrom = time.Time{}
	} else {
		b.waitingFrom = now
	}
	return ready
}

func seqDistance(from, to byte) int {
	return int((uint16(to) - uint16(from)) & 0xFF)
}

type rxSequenceTracker struct {
	initialized bool
	expected    byte
}

func (t *rxSequenceTracker) observe(packet []byte) rxReorderStats {
	var stats rxReorderStats
	if !prtp.IsAudioPacket(packet) {
		return stats
	}
	seq := packet[1]
	if !t.initialized {
		t.initialized = true
		t.expected = seq + 1
		return stats
	}
	if seq == t.expected {
		t.expected++
		return stats
	}
	delta := seqDistance(t.expected, seq)
	if delta > 0 && delta < 128 {
		stats.gapEvents++
		stats.missing += uint64(delta)
		t.expected = seq + 1
		return stats
	}
	stats.stale++
	return stats
}

type WSConfig struct {
	SampleRate   int
	Channels     int
	FrameSamples int
}

func Serve(ctx context.Context, cfg config.Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	txSource, ok := config.NormalizeAudioSource(cfg.Audio.Source)
	if !ok {
		return fmt.Errorf("invalid audio source %q", cfg.Audio.Source)
	}
	tlsEnabled, err := tlsEnabled(cfg.TLS)
	if err != nil {
		return err
	}
	if (cfg.Audio.LocalPlayback || cfg.Audio.LocalCapture || txSource == "server") && !audio.Supported() {
		return fmt.Errorf("local audio playback/recording is not supported on this build")
	}

	var txSourceMu sync.RWMutex
	getTXSource := func() string {
		txSourceMu.RLock()
		defer txSourceMu.RUnlock()
		return txSource
	}
	setTXSource := func(source string) bool {
		source, ok := config.NormalizeAudioSource(source)
		if !ok {
			return false
		}
		txSourceMu.Lock()
		txSource = source
		txSourceMu.Unlock()
		return true
	}

	emulationAuto := emulationDeviceAuto(cfg.Emulation.Device)
	var emu *prtp.EmulationProfile
	if emulationAuto {
		log.Printf("PRTP emulation auto-detect enabled from matrix port %s", cfg.MatrixPort)
	} else {
		emu, err = prtp.NewEmulationProfile(cfg.Emulation.Device, cfg.Emulation.Name, cfg.Emulation.Keys)
		if err != nil {
			return err
		}
		if emu != nil {
			if err := prtp.ApplyEmulationVersions(emu, cfg.Emulation.UserVersion, cfg.Emulation.FPGAVersion); err != nil {
				return err
			}
			log.Printf("PRTP emulation enabled: %s %q (%s, %d keys, type=0x%02X, user=%s, fpga=%s)", emu.Model, emu.Name, emu.Kind, emu.KeyCount, emu.TypeCode, prtp.FormatUserVersion(emu.UserVersion), prtp.FormatFPGAVersion(emu.FPGAVersion))
		}
	}
	matrixTarget, err := matrix.ParsePortRef(cfg.MatrixPort)
	if err != nil {
		return err
	}
	helper := matrixhelper.NewClient(cfg.MatrixHelperSocket)

	codec, codecSource, err := g711.New(cfg.G711.Mode, cfg.G711.Table)
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.G711.TXMap) != "" {
		txMap, err := g711.LoadTXMap(cfg.G711.TXMap)
		if err != nil {
			return err
		}
		codec.SetTXMap(txMap)
		log.Printf("using G.711 outbound TX remap (%s)", cfg.G711.TXMap)
	}
	if codec.Mode() == "custom" {
		log.Printf("using custom G.711 table (%s)", codecSource)
	} else {
		log.Printf("using standard A-law G.711")
	}
	if cfg.Audio.FIRFilter {
		log.Printf("TX FIR anti-aliasing enabled (%d taps; applied only before downsampling)", len(audio.DefaultFIRCoeffs))
	}

	udpConn, err := net.ListenPacket("udp", cfg.UDP.Bind)
	if err != nil {
		return err
	}
	defer udpConn.Close()
	log.Printf("UDP bind %s", cfg.UDP.Bind)

	var peerMu sync.RWMutex
	var udpPeerAddr net.Addr
	if cfg.UDP.Peer != "" {
		udpPeerAddr, err = net.ResolveUDPAddr("udp", cfg.UDP.Peer)
		if err != nil {
			return err
		}
		log.Printf("UDP peer preset %s", cfg.UDP.Peer)
	}
	getPeer := func() net.Addr {
		peerMu.RLock()
		defer peerMu.RUnlock()
		return udpPeerAddr
	}
	setPeer := func(addr net.Addr) (learned bool) {
		if cfg.UDP.Peer != "" || addr == nil {
			return false
		}
		peerMu.Lock()
		defer peerMu.Unlock()
		learned = udpPeerAddr == nil
		udpPeerAddr = addr
		return learned
	}

	rxPCMSpec := firstNonEmpty(cfg.Dump.MatrixRxPCM)
	txPCMSpec := firstNonEmpty(cfg.Dump.MatrixTxPCM)
	rxG711Spec := firstNonEmpty(cfg.Dump.MatrixRxG711)
	txG711Spec := firstNonEmpty(cfg.Dump.MatrixTxG711)
	if cfg.Dump.MatrixDir != "" {
		if rxPCMSpec == "" {
			rxPCMSpec = cfg.Dump.MatrixDir
		}
		if txPCMSpec == "" {
			txPCMSpec = cfg.Dump.MatrixDir
		}
		if rxG711Spec == "" {
			rxG711Spec = cfg.Dump.MatrixDir
		}
		if txG711Spec == "" {
			txG711Spec = cfg.Dump.MatrixDir
		}
	}
	rxPCMDump, err := audio.OpenDumpSink(rxPCMSpec, "matrix-rx", "s16le")
	if err != nil {
		return fmt.Errorf("open matrix RX PCM dump: %w", err)
	}
	if rxPCMDump != nil {
		defer rxPCMDump.Close()
		log.Printf("matrix RX PCM dump enabled to %s (decoded raw s16le, %d Hz, mono)", rxPCMDump.Path(), cfg.UDP.Rate)
	}
	txPCMDump, err := audio.OpenDumpSink(txPCMSpec, "matrix-tx", "s16le")
	if err != nil {
		return fmt.Errorf("open matrix TX PCM dump: %w", err)
	}
	if txPCMDump != nil {
		defer txPCMDump.Close()
		log.Printf("matrix TX PCM dump enabled to %s (raw s16le, %d Hz, mono)", txPCMDump.Path(), cfg.UDP.Rate)
	}
	rxG711Dump, err := audio.OpenDumpSink(rxG711Spec, "matrix-rx", "g711")
	if err != nil {
		return fmt.Errorf("open matrix RX G.711 dump: %w", err)
	}
	if rxG711Dump != nil {
		defer rxG711Dump.Close()
		log.Printf("matrix RX G.711 dump enabled to %s (raw 256-byte payloads)", rxG711Dump.Path())
	}
	txG711Dump, err := audio.OpenDumpSink(txG711Spec, "matrix-tx", "g711")
	if err != nil {
		return fmt.Errorf("open matrix TX G.711 dump: %w", err)
	}
	if txG711Dump != nil {
		defer txG711Dump.Close()
		log.Printf("matrix TX G.711 dump enabled to %s (raw 256-byte payloads)", txG711Dump.Path())
	}

	h := newHub()
	wsCfg := WSConfig{SampleRate: cfg.UDP.Rate, Channels: 1, FrameSamples: serverFrameSamples}
	var wsInputRate int64
	atomic.StoreInt64(&wsInputRate, int64(wsCfg.SampleRate))

	var rxFrames, txFrames, rxBytes, txBytes, prtpControlFrames, prtpCRCErrors uint64
	var rxSeqGapEvents, rxReordered, rxSeqMissing, rxStale uint64
	var rxFragmentsRecovered, rxFragmentsDropped uint64
	var sentSync bool
	var emuMu sync.RWMutex
	var emuState *prtp.EmulationState
	if emu != nil {
		emuState = &prtp.EmulationState{}
	}
	getEmulation := func() (*prtp.EmulationProfile, *prtp.EmulationState) {
		emuMu.RLock()
		defer emuMu.RUnlock()
		return emu, emuState
	}
	setEmulation := func(next *prtp.EmulationProfile, reason string) (*prtp.EmulationProfile, *prtp.EmulationState, bool) {
		if next == nil {
			return nil, nil, false
		}
		emuMu.Lock()
		defer emuMu.Unlock()
		if emu != nil &&
			emu.Model == next.Model &&
			emu.Kind == next.Kind &&
			emu.Name == next.Name &&
			emu.TypeCode == next.TypeCode &&
			emu.KeyCount == next.KeyCount &&
			emu.UserVersion == next.UserVersion &&
			emu.FPGAVersion == next.FPGAVersion {
			return emu, emuState, false
		}
		emu = next
		emuState = &prtp.EmulationState{}
		log.Printf("PRTP emulation selected: %s %q (%s, %d keys, type=0x%02X, user=%s, fpga=%s, reason=%s)", emu.Model, emu.Name, emu.Kind, emu.KeyCount, emu.TypeCode, prtp.FormatUserVersion(emu.UserVersion), prtp.FormatFPGAVersion(emu.FPGAVersion), reason)
		return emu, emuState, true
	}
	var sendAudioBlock func([]int16)
	var queueAudioBlock func([]int16, bool)
	var ctrlMu sync.Mutex
	var ctrlQueue prtpControlQueue
	txAudioQueue := make(chan []int16, txQueueFrames)
	frameDuration := time.Duration(float64(time.Second) * serverFrameSamples / float64(cfg.UDP.Rate))
	if frameDuration <= 0 {
		frameDuration = 31 * time.Millisecond
	}
	recentAudioWindow := 2 * frameDuration
	var lastRealAudioNano int64

	prtpPending := func() int {
		ctrlMu.Lock()
		defer ctrlMu.Unlock()
		return ctrlQueue.pendingBytes()
	}
	prtpEnqueue := func(frame []byte, priority prtpControlPriority) int {
		if len(frame) == 0 {
			return prtpPending()
		}
		ctrlMu.Lock()
		ctrlQueue.enqueue(frame, priority)
		n := ctrlQueue.pendingBytes()
		ctrlMu.Unlock()
		return n
	}
	prtpPop := func(max int) []byte {
		if max <= 0 {
			return nil
		}
		ctrlMu.Lock()
		defer ctrlMu.Unlock()
		return ctrlQueue.pop(max)
	}

	queueControlDrain := func(pending int) {
		if pending <= 0 || getPeer() == nil {
			return
		}
		currentSource := getTXSource()
		if currentSource == "silence" || currentSource == "tone" {
			return
		}
		if lastReal := atomic.LoadInt64(&lastRealAudioNano); lastReal != 0 && time.Since(time.Unix(0, lastReal)) <= recentAudioWindow {
			return
		}
		if queueAudioBlock == nil {
			return
		}
		silence := make([]int16, serverFrameSamples)
		queueAudioBlock(silence, false)
	}
	sendPRTPFramePriority := func(frame []byte, priority prtpControlPriority) {
		queueControlDrain(prtpEnqueue(frame, priority))
	}
	sendPRTPFrame := func(frame []byte) {
		sendPRTPFramePriority(frame, prtpControlHigh)
	}
	sendPRTPFrameNormal := func(frame []byte) {
		sendPRTPFramePriority(frame, prtpControlNormal)
	}
	applyAutoEmulation := func(snap *matrix.NameSnapshot, reason string) {
		if !emulationAuto || snap == nil {
			return
		}
		next, ok, err := emulationProfileFromMatrixTarget(snap.Target, cfg.Emulation)
		if err != nil {
			log.Printf("PRTP emulation auto-detect failed: %v", err)
			return
		}
		if !ok {
			log.Printf("PRTP emulation auto-detect skipped: matrix target %s type=0x%02X (%s) is not a supported PRTP emulation model", snap.Target.Port, snap.Target.TypeCode, snap.Target.TypeName)
			return
		}
		cur, state, changed := setEmulation(next, reason)
		if !changed {
			return
		}
		if getPeer() != nil {
			announceEmulationOnce(h, cur, state, sendPRTPFrameNormal, reason, cfg.Debug.PRTP)
			return
		}
		publishEmulationSnapshot(cur, h.broadcastJSON)
	}
	publishMatrixSnapshot := func(snap *matrix.NameSnapshot) {
		msg := matrixNamesMessage(snap)
		h.broadcastJSON(msg)
		for i, label := range snap.ButtonLabels {
			if strings.TrimSpace(label) == "" {
				continue
			}
			h.broadcastJSON(map[string]any{"type": "prtp_info", "kind": "label", "index": i, "text": label})
		}
	}
	if emulationAuto && strings.TrimSpace(cfg.MatrixAddr) != "" {
		go func() {
			addr := matrix.NormalizeAddr(cfg.MatrixAddr)
			reqCtx, cancel := context.WithTimeout(ctx, helperRequestTimeout)
			defer cancel()
			log.Printf("matrix names auto-fetch for emulation: addr=%s target=%s(%d)", addr, matrixTarget.Port, matrixTarget.Index)
			snap, err := helper.Names(reqCtx, matrixhelper.NamesRequest{Addr: addr, MatrixPort: matrixTarget.Port})
			if err != nil {
				log.Printf("matrix names auto-fetch for emulation failed: %v", err)
				return
			}
			applyAutoEmulation(snap, "matrix-auto-startup")
			publishMatrixSnapshot(snap)
		}()
	}

	var txSeq byte
	writeAudioBlock := func(block []int16) {
		if len(block) == 0 {
			return
		}
		if txPCMDump != nil {
			txPCMDump.WriteInt16(block)
		}
		g := codec.Encode(block)
		if txG711Dump != nil {
			txG711Dump.Write(g)
		}
		ctrl := prtpPop(4)
		d := prtp.BuildAudioPacket(txSeq, g, ctrl)
		txSeq++
		if peer := getPeer(); peer != nil {
			if _, err := udpConn.WriteTo(d, peer); err == nil {
				atomic.AddUint64(&txFrames, 1)
				atomic.AddUint64(&txBytes, uint64(len(d)))
			}
		}
	}
	go func() {
		ticker := time.NewTicker(frameDuration)
		defer ticker.Stop()
		controlSilence := make([]int16, serverFrameSamples)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				select {
				case block := <-txAudioQueue:
					writeAudioBlock(block)
				default:
					if prtpPending() > 0 && getPeer() != nil {
						writeAudioBlock(controlSilence)
					}
				}
			}
		}
	}()

	queueAudioBlock = func(block []int16, realAudio bool) {
		if len(block) == 0 || getPeer() == nil {
			return
		}
		queued := make([]int16, serverFrameSamples)
		copy(queued, block)
		if realAudio {
			atomic.StoreInt64(&lastRealAudioNano, time.Now().UnixNano())
		}
		select {
		case txAudioQueue <- queued:
		default:
			for {
				select {
				case <-txAudioQueue:
					continue
				default:
				}
				break
			}
			select {
			case txAudioQueue <- queued:
			default:
			}
		}
	}
	queueControlDrain(prtpPending())
	sendAudioBlock = func(block []int16) {
		queueAudioBlock(block, true)
	}

	if txSource == "silence" || txSource == "tone" {
		go func(sourceName string) {
			var source audio.Source = audio.SilenceSource{}
			if sourceName == "tone" {
				source = audio.NewToneSource(cfg.UDP.Rate, 1, cfg.Audio.ToneFreq)
			}
			ticker := time.NewTicker(frameDuration)
			defer ticker.Stop()
			block := make([]int16, serverFrameSamples)
			log.Printf("gateway backend audio source enabled: %s", sourceName)
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if getTXSource() != sourceName {
						continue
					}
					source.Next(block)
					sendAudioBlock(block)
				}
			}
		}(txSource)
	} else if txSource == "echo" {
		log.Printf("gateway backend audio source enabled: echo matrix RX back to matrix TX")
	}

	if curEmu, curState := getEmulation(); curEmu != nil && getPeer() != nil {
		announceEmulationOnce(h, curEmu, curState, sendPRTPFrameNormal, "preset-peer", cfg.Debug.PRTP)
	}

	var (
		localMu                sync.RWMutex
		local                  audio.Local
		localGeneration        uint64
		serverAudioClientName  = audioClientName(cfg)
		serverCaptureDeviceID  = audio.DefaultDeviceID
		serverPlaybackDeviceID = audio.DefaultDeviceID
		serverCaptureEnabled   bool
		serverPlaybackEnabled  bool
	)
	serverPlaybackQueue := make(chan []int16, rxPlaybackFrames)
	queueServerPlayback := func(pcm []int16) {
		if len(pcm) == 0 {
			return
		}
		frame := make([]int16, len(pcm))
		copy(frame, pcm)
		select {
		case serverPlaybackQueue <- frame:
			return
		default:
		}
		select {
		case <-serverPlaybackQueue:
		default:
		}
		select {
		case serverPlaybackQueue <- frame:
		default:
		}
	}
	go func() {
		var (
			generation uint64
			outRate    int
			resampler  *audio.Int16StreamResampler
		)
		for {
			select {
			case <-ctx.Done():
				return
			case pcm := <-serverPlaybackQueue:
				localMu.RLock()
				playback := local
				enabled := playback != nil && playback.PlaybackEnabled()
				rate := 0
				gen := localGeneration
				if enabled {
					rate = playback.SampleRate()
				}
				localMu.RUnlock()
				if !enabled || rate <= 0 {
					resampler = nil
					generation = gen
					outRate = 0
					continue
				}
				if resampler == nil || generation != gen || outRate != rate {
					resampler = audio.NewInt16StreamResampler(cfg.UDP.Rate, rate)
					generation = gen
					outRate = rate
				}
				out := resampler.Process(pcm)
				if len(out) > 0 {
					playback.Play(out)
				}
			}
		}
	}()
	defer func() {
		localMu.Lock()
		defer localMu.Unlock()
		if local != nil {
			_ = local.Close()
			local = nil
		}
	}()

	serverAudioState := func() (map[string]string, map[string]bool, map[string]bool) {
		localMu.RLock()
		defer localMu.RUnlock()
		selected := map[string]string{
			"capture":  serverCaptureDeviceID,
			"playback": serverPlaybackDeviceID,
		}
		enabled := map[string]bool{
			"capture":  serverCaptureEnabled,
			"playback": serverPlaybackEnabled,
		}
		running := map[string]bool{
			"capture":  local != nil && local.CaptureEnabled(),
			"playback": local != nil && local.PlaybackEnabled(),
		}
		return selected, enabled, running
	}
	serverAudioStatusMessage := func(ok bool, statusErr error) map[string]any {
		selected, enabled, running := serverAudioState()
		msg := map[string]any{
			"type":        "server_audio_status",
			"ok":          ok,
			"backend":     ternary(audio.Supported(), audio.BackendName(), "unsupported"),
			"supported":   audio.Supported(),
			"client_name": serverAudioClientName,
			"selected":    selected,
			"enabled":     enabled,
			"running":     running,
			"tx_source":   getTXSource(),
		}
		if statusErr != nil {
			msg["error"] = statusErr.Error()
		}
		return msg
	}
	serverAudioDevicesMessage := func() map[string]any {
		snap, err := audio.ListDevices()
		selected, enabled, running := serverAudioState()
		msg := map[string]any{
			"type":        "server_audio_devices",
			"supported":   snap.Supported,
			"backend":     snap.Backend,
			"client_name": serverAudioClientName,
			"capture":     snap.Capture,
			"playback":    snap.Playback,
			"selected":    selected,
			"enabled":     enabled,
			"running":     running,
			"tx_source":   getTXSource(),
		}
		if err != nil {
			msg["ok"] = false
			msg["error"] = err.Error()
		} else {
			msg["ok"] = true
		}
		return msg
	}
	startLocalCapturePump := func(la audio.Local) {
		if la == nil || !la.CaptureEnabled() {
			return
		}
		localTXFIR := audio.NewTXAntiAliasFIR(cfg.Audio.FIRFilter, la.SampleRate(), cfg.UDP.Rate)
		if localTXFIR != nil {
			log.Printf("server capture TX FIR active before downsampling %d Hz -> %d Hz", la.SampleRate(), cfg.UDP.Rate)
		}
		go func() {
			var pending []int16
			for {
				select {
				case <-ctx.Done():
					return
				case frame, ok := <-la.RecordChan():
					if !ok {
						return
					}
					if len(frame) == 0 || getTXSource() != "server" {
						continue
					}
					input := frame
					if localTXFIR != nil {
						input = append([]int16(nil), frame...)
						localTXFIR.Process(input)
					}
					res := audio.ResampleInt16(input, la.SampleRate(), cfg.UDP.Rate)
					pending = append(pending, res...)
					for len(pending) >= serverFrameSamples {
						block := make([]int16, serverFrameSamples)
						copy(block, pending[:serverFrameSamples])
						pending = pending[serverFrameSamples:]
						sendAudioBlock(block)
					}
				}
			}
		}()
	}
	applyServerAudio := func(captureEnabled, playbackEnabled bool, captureID, playbackID string) error {
		captureID = audio.NormalizeDeviceID(captureID)
		playbackID = audio.NormalizeDeviceID(playbackID)
		if (captureEnabled || playbackEnabled) && !audio.Supported() {
			return errors.New("server audio is not supported on this build")
		}

		var next audio.Local
		var err error
		localMu.Lock()
		if local != nil {
			_ = local.Close()
			local = nil
		}
		localGeneration++
		serverCaptureDeviceID = captureID
		serverPlaybackDeviceID = playbackID
		serverCaptureEnabled = false
		serverPlaybackEnabled = false
		if captureEnabled || playbackEnabled {
			next, err = audio.NewLocalWithOptions(audio.LocalOptions{
				Playback:     playbackEnabled,
				Capture:      captureEnabled,
				SampleRate:   cfg.Audio.LocalRate,
				FrameSamples: serverFrameSamples,
				PlaybackID:   playbackID,
				CaptureID:    captureID,
				ClientName:   serverAudioClientName,
			})
			if err == nil {
				local = next
				serverCaptureEnabled = captureEnabled
				serverPlaybackEnabled = playbackEnabled
			}
		}
		localMu.Unlock()
		if err != nil {
			return err
		}
		startLocalCapturePump(next)
		return nil
	}

	initialServerCapture := cfg.Audio.LocalCapture || txSource == "server"
	if initialServerCapture {
		setTXSource("server")
	}
	if cfg.Audio.LocalPlayback || initialServerCapture {
		if err := applyServerAudio(initialServerCapture, cfg.Audio.LocalPlayback, cfg.Audio.CaptureID, cfg.Audio.PlaybackID); err != nil {
			return fmt.Errorf("init server audio: %w", err)
		}
		log.Printf("server audio enabled: backend=%s client=%s capture=%t playback=%t source=%s", audio.BackendName(), serverAudioClientName, initialServerCapture, cfg.Audio.LocalPlayback, getTXSource())
	}

	controlWSPath, err := config.NormalizeWSPath(cfg.WebSocketPaths.Control, "websocket_paths.control")
	if err != nil {
		return err
	}
	audioWSPath, err := config.NormalizeWSPath(cfg.WebSocketPaths.Audio, "websocket_paths.audio")
	if err != nil {
		return err
	}
	if controlWSPath == audioWSPath {
		return fmt.Errorf("websocket paths must be unique: control=%s audio=%s", controlWSPath, audioWSPath)
	}

	mux := http.NewServeMux()
	mux.Handle("/", noCacheStatic(http.FileServer(http.FS(webui.FS()))))
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	writeInitialControlState := func(cli *client) {
		initial := map[string]any{
			"type":                   "config",
			"sample_rate":            wsCfg.SampleRate,
			"channels":               1,
			"frame_samples":          wsCfg.FrameSamples,
			"server_audio_supported": audio.Supported(),
			"server_audio_backend":   ternary(audio.Supported(), audio.BackendName(), "unsupported"),
			"tx_source":              getTXSource(),
			"control_ws_path":        controlWSPath,
			"audio_ws_path":          audioWSPath,
			"matrix_helper_socket":   cfg.MatrixHelperSocket,
			"matrix_port":            matrixTarget.Port,
			"matrix_port_index":      matrixTarget.Index,
		}
		if strings.TrimSpace(cfg.MatrixAddr) != "" {
			initial["matrix_addr"] = matrix.NormalizeAddr(cfg.MatrixAddr)
		}
		curEmu, _ := getEmulation()
		if curEmu != nil {
			initial["emulate_device"] = curEmu.Model
			initial["emulate_kind"] = curEmu.Kind
			initial["emulate_name"] = curEmu.Name
			initial["emulate_keys"] = curEmu.KeyCount
			initial["emulate_type_code"] = int(curEmu.TypeCode)
			initial["emulate_user_version"] = prtp.FormatUserVersion(curEmu.UserVersion)
			initial["emulate_fpga_version"] = prtp.FormatFPGAVersion(curEmu.FPGAVersion)
		}
		cli.writeJSON(initial)
		cli.writeJSON(serverAudioDevicesMessage())
		cli.writeJSON(serverAudioStatusMessage(true, nil))
		if !h.hasSnapshotKey("prtp_emulation") {
			publishEmulationSnapshot(curEmu, cli.writeJSON)
		}
		h.replaySnapshot(cli)
	}

	handleControlText := func(cli *client, msg []byte) {
		var m map[string]any
		if json.Unmarshal(msg, &m) != nil {
			return
		}
		switch strings.ToLower(fmt.Sprint(m["type"])) {
		case "config":
			if rate, ok := jsonMapInt(m, "sample_rate"); ok && rate > 0 {
				atomic.StoreInt64(&wsInputRate, int64(rate))
			}
			if source, ok := jsonMapString(m, "tx_source"); ok {
				if _, valid := config.NormalizeAudioSource(source); valid {
					setTXSource(source)
					h.broadcastJSON(serverAudioStatusMessage(true, nil))
				} else {
					cli.writeJSON(serverAudioStatusMessage(false, fmt.Errorf("invalid tx_source %q", source)))
				}
			}
		case "server_audio_refresh":
			cli.writeJSON(serverAudioDevicesMessage())
			cli.writeJSON(serverAudioStatusMessage(true, nil))
		case "server_audio_select":
			selected, enabled, _ := serverAudioState()
			captureID := selected["capture"]
			playbackID := selected["playback"]
			captureEnabled := enabled["capture"]
			playbackEnabled := enabled["playback"]
			if s, ok := jsonMapString(m, "capture_id"); ok {
				captureID = s
			}
			if s, ok := jsonMapString(m, "playback_id"); ok {
				playbackID = s
			}
			if v, ok := jsonMapBool(m, "capture_enabled"); ok {
				captureEnabled = v
			}
			if v, ok := jsonMapBool(m, "playback_enabled"); ok {
				playbackEnabled = v
			}
			source, hasSource := jsonMapString(m, "tx_source")
			if !hasSource && captureEnabled {
				source = "server"
				hasSource = true
			}
			if hasSource {
				if _, valid := config.NormalizeAudioSource(source); !valid {
					cli.writeJSON(serverAudioStatusMessage(false, fmt.Errorf("invalid tx_source %q", source)))
					return
				}
			}
			if err := applyServerAudio(captureEnabled, playbackEnabled, captureID, playbackID); err != nil {
				log.Printf("server audio select failed: %v", err)
				cli.writeJSON(serverAudioStatusMessage(false, err))
				return
			}
			if hasSource {
				setTXSource(source)
			}
			h.broadcastJSON(serverAudioStatusMessage(true, nil))
		case "prtp_control":
			var frames [][]byte
			if s, _ := m["b64"].(string); s != "" {
				if raw, err := base64.StdEncoding.DecodeString(s); err == nil && len(raw) > 0 {
					frames = append(frames, raw)
				}
			}
			if arr, ok := m["bytes"].([]any); ok {
				bs := make([]byte, 0, len(arr))
				for _, v := range arr {
					if f, ok := v.(float64); ok {
						bs = append(bs, byte(int(f)&0xFF))
					}
				}
				if len(bs) > 0 {
					frames = append(frames, bs)
				}
			}
			for _, frame := range frames {
				if cfg.Debug.PRTP {
					log.Printf("PRTP outbound raw=[%s]", prtp.HexBytes(frame))
				}
				sendPRTPFrame(frame)
			}
		case "prtp_send":
			cmd, _ := m["cmd"].(string)
			var payload []byte
			switch cmd {
			case "KEY":
				idx := 0
				if v, ok := m["index"].(float64); ok {
					idx = int(v)
				}
				pressed := false
				if v, ok := m["pressed"].(bool); ok {
					pressed = v
				}
				b2 := byte(idx & 0x7F)
				if pressed {
					b2 |= 0x80
				}
				payload = []byte{0x49, 0x00, b2}
			}
			if len(payload) > 0 {
				frame := prtp.BuildFrame(payload)
				if cfg.Debug.PRTP {
					log.Printf("PRTP outbound payload=[%s] frame=[%s]", prtp.HexBytes(payload), prtp.HexBytes(frame))
				}
				sendPRTPFrame(frame)
			}
		case "matrix_fetch_names":
			addr := matrixAddrFromMessage(m, cfg.MatrixAddr)
			if addr == "" {
				cli.writeJSON(map[string]any{"type": "matrix_names", "ok": false, "error": "matrix address is required"})
				return
			}
			go func(addr string) {
				log.Printf("matrix names fetch through helper: addr=%s target=%s(%d)", addr, matrixTarget.Port, matrixTarget.Index)
				reqCtx, cancel := context.WithTimeout(ctx, helperRequestTimeout)
				defer cancel()
				snap, err := helper.Names(reqCtx, matrixhelper.NamesRequest{Addr: addr, MatrixPort: matrixTarget.Port})
				if err != nil {
					log.Printf("matrix names fetch failed: %v", err)
					cli.writeJSON(map[string]any{"type": "matrix_names", "ok": false, "addr": addr, "error": err.Error()})
					return
				}
				applyAutoEmulation(snap, "matrix-auto-request")
				publishMatrixSnapshot(snap)
			}(addr)
		case "matrix_crosspoint", "matrix_set_crosspoint":
			addr := matrixAddrFromMessage(m, cfg.MatrixAddr)
			xin, xinOK := jsonMapInt(m, "xin")
			xout, xoutOK := jsonMapInt(m, "xout")
			enabled, enabledOK := jsonMapBool(m, "enabled")
			save, _ := jsonMapBool(m, "save")
			if addr == "" || !xinOK || !xoutOK || !enabledOK {
				cli.writeJSON(map[string]any{"type": "matrix_crosspoint", "ok": false, "error": "addr, xin, xout, and enabled are required"})
				return
			}
			go func() {
				reqCtx, cancel := context.WithTimeout(ctx, helperRequestTimeout)
				defer cancel()
				resp, err := helper.Crosspoint(reqCtx, matrixhelper.CrosspointRequest{Addr: addr, XIn: xin, XOut: xout, Enabled: enabled, Save: save})
				if err != nil {
					cli.writeJSON(map[string]any{"type": "matrix_crosspoint", "ok": false, "error": err.Error()})
					return
				}
				msg := map[string]any{"type": "matrix_crosspoint", "ok": resp.OK}
				if resp.Code != "" {
					msg["code"] = resp.Code
				}
				if resp.Error != "" {
					msg["error"] = resp.Error
				}
				cli.writeJSON(msg)
			}()
		case "ping":
			cli.writeJSON(map[string]any{"type": "pong", "t": m["t"], "now": time.Now().UnixMilli()})
		}
	}

	handleAudioBinary := func(state *wsAudioInputState, msg []byte) {
		if len(msg)%2 != 0 || getTXSource() != "ws" {
			return
		}
		inputRate := int(atomic.LoadInt64(&wsInputRate))
		if inputRate <= 0 {
			inputRate = cfg.UDP.Rate
		}
		if state.resampler == nil || state.inputRate != inputRate {
			state.inputRate = inputRate
			state.pcmBuf = state.pcmBuf[:0]
			state.resampler = audio.NewInt16StreamResampler(inputRate, cfg.UDP.Rate)
			state.fir = audio.NewTXAntiAliasFIR(cfg.Audio.FIRFilter, inputRate, cfg.UDP.Rate)
			if state.fir != nil {
				log.Printf("websocket TX FIR active before downsampling %d Hz -> %d Hz", inputRate, cfg.UDP.Rate)
			}
		}
		n := len(msg) / 2
		tmp := make([]int16, n)
		for i := 0; i < n; i++ {
			tmp[i] = int16(binary.LittleEndian.Uint16(msg[2*i:]))
		}
		if state.fir != nil {
			state.fir.Process(tmp)
		}
		tmp = state.resampler.Process(tmp)
		state.pcmBuf = append(state.pcmBuf, tmp...)
		for len(state.pcmBuf) >= serverFrameSamples {
			block := make([]int16, serverFrameSamples)
			copy(block, state.pcmBuf[:serverFrameSamples])
			state.pcmBuf = state.pcmBuf[serverFrameSamples:]
			sendAudioBlock(block)
		}
	}

	serveWS := func(path string, control, audioStream bool) {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			c, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			cli := newClient(c, control, audioStream)
			h.add(cli)
			go cli.writeLoop()
			defer h.remove(cli)
			if control {
				writeInitialControlState(cli)
			}
			state := &wsAudioInputState{}
			for {
				mt, msg, err := c.ReadMessage()
				if err != nil {
					return
				}
				switch mt {
				case websocket.TextMessage:
					if control {
						handleControlText(cli, msg)
					}
				case websocket.BinaryMessage:
					if audioStream {
						handleAudioBinary(state, msg)
					}
				}
			}
		})
	}
	serveWS(controlWSPath, true, false)
	serveWS(audioWSPath, false, true)

	go func() {
		var prtpEsc bool
		var prtpAcc []byte
		var fragments rxFragmentAssembler
		reorderWait := time.Duration(cfg.UDP.RXReorderMS) * time.Millisecond
		var reorder *rxReorderBuffer
		var seqTracker rxSequenceTracker
		if reorderWait > 0 {
			reorder = newRXReorderBuffer(reorderWait)
			log.Printf("UDP RX sequence reorder buffer enabled: wait=%s", reorderWait)
		} else {
			log.Printf("UDP RX sequence reorder buffer disabled")
		}
		recordReorderStats := func(stats rxReorderStats) {
			if stats.gapEvents > 0 {
				atomic.AddUint64(&rxSeqGapEvents, stats.gapEvents)
			}
			if stats.reordered > 0 {
				atomic.AddUint64(&rxReordered, stats.reordered)
			}
			if stats.missing > 0 {
				atomic.AddUint64(&rxSeqMissing, stats.missing)
			}
			if stats.stale > 0 {
				atomic.AddUint64(&rxStale, stats.stale)
			}
		}
		flushReorder := func(now time.Time) []rxBufferedPacket {
			var stats rxReorderStats
			ready := reorder.flushExpired(now, &stats)
			recordReorderStats(stats)
			return ready
		}
		recordFragmentStats := func(stats rxFragmentStats) {
			if stats.recovered > 0 {
				atomic.AddUint64(&rxFragmentsRecovered, stats.recovered)
			}
			if stats.dropped > 0 {
				atomic.AddUint64(&rxFragmentsDropped, stats.dropped)
			}
		}
		processPacket := func(addr net.Addr, packet []byte) {
			learnedPeer := setPeer(addr)
			sawControl := false
			if prtp.IsAudioPacket(packet) {
				payload := packet[prtp.AudioPayloadOffset : prtp.AudioPayloadOffset+prtp.AudioPayloadSize]
				if rxG711Dump != nil {
					rxG711Dump.Write(payload)
				}
				pcm := codec.Decode(payload)
				if rxPCMDump != nil {
					rxPCMDump.WriteInt16(pcm)
				}
				if getTXSource() == "echo" {
					echo := make([]int16, len(pcm))
					copy(echo, pcm)
					sendAudioBlock(echo)
				}
				bin := make([]byte, len(pcm)*2)
				for i, v := range pcm {
					binary.LittleEndian.PutUint16(bin[2*i:], uint16(v))
				}
				localMu.RLock()
				playbackActive := local != nil && local.PlaybackEnabled()
				localMu.RUnlock()
				if playbackActive {
					queueServerPlayback(pcm)
				}
				h.broadcast(bin)
				atomic.AddUint64(&rxFrames, 1)
				atomic.AddUint64(&rxBytes, uint64(len(packet)))
				if (packet[5] & 0x04) != 0 {
					nctrl := int(packet[7])
					if nctrl > 0 && nctrl <= 4 {
						sawControl = true
						ctrl := make([]byte, nctrl)
						copy(ctrl, packet[8:8+nctrl])
						if cfg.Debug.PRTP {
							log.Printf("PRTP inbound ctrl=[%s]", prtp.HexBytes(ctrl))
						}
						h.broadcastJSON(map[string]any{"type": "prtp_control", "b64": base64.StdEncoding.EncodeToString(ctrl), "n": nctrl})
						for _, b := range ctrl {
							if prtpEsc {
								prtpAcc = append(prtpAcc, b)
								prtpEsc = false
								continue
							}
							if b == 0xFE {
								prtpEsc = true
								continue
							}
							if b == 0xFF {
								if len(prtpAcc) >= 2 {
									frame := append([]byte(nil), prtpAcc...)
									rawPayload := frame[:len(frame)-1]
									payload, ok := prtp.DecodeFrame(frame)
									if !ok {
										atomic.AddUint64(&prtpCRCErrors, 1)
										if cfg.Debug.PRTP {
											log.Printf("PRTP inbound frame raw=[%s] CRC mismatch expect=%02X (discarding)", prtp.HexBytes(rawPayload), prtp.CRC(rawPayload))
										}
										prtpAcc = prtpAcc[:0]
										continue
									}
									if cfg.Debug.PRTP {
										log.Printf("PRTP inbound frame raw=[%s] (CRC ok)", prtp.HexBytes(rawPayload))
									}
									atomic.AddUint64(&prtpControlFrames, 1)
									curEmu, _ := getEmulation()
									emitInfo(h, payload, sendPRTPFrame, sendPRTPFrameNormal, &sentSync, curEmu, cfg.Debug.PRTP)
								}
								prtpAcc = prtpAcc[:0]
								continue
							}
							prtpAcc = append(prtpAcc, b)
						}
					}
				}
			}
			if learnedPeer && !sawControl {
				curEmu, curState := getEmulation()
				announceEmulationOnce(h, curEmu, curState, sendPRTPFrameNormal, "first-peer", cfg.Debug.PRTP)
			}
		}
		dispatchPacket := func(now time.Time, addr net.Addr, packet []byte) {
			ready, fragmentStats := fragments.push(now, addr, packet)
			recordFragmentStats(fragmentStats)
			for _, item := range ready {
				if reorder == nil {
					recordReorderStats(seqTracker.observe(item.packet))
					processPacket(item.addr, item.packet)
					continue
				}
				packets, stats := reorder.push(now, item.addr, item.packet)
				recordReorderStats(stats)
				for _, queued := range packets {
					processPacket(queued.addr, queued.packet)
				}
			}
		}
		buf := make([]byte, 2048)
		for {
			deadline := time.Now().Add(1 * time.Second)
			if reorder != nil && reorder.hasPending() {
				deadline = time.Now().Add(reorderWait)
			}
			_ = udpConn.SetReadDeadline(deadline)
			n, addr, err := udpConn.ReadFrom(buf)
			if err != nil {
				select {
				case <-ctx.Done():
					return
				default:
				}
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					if reorder != nil {
						for _, queued := range flushReorder(time.Now()) {
							processPacket(queued.addr, queued.packet)
						}
					}
					continue
				}
				continue
			}
			packet := buf[:n]
			dispatchPacket(time.Now(), addr, packet)
		}
	}()

	srv := &http.Server{
		Addr:      cfg.Listen,
		Handler:   mux,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
	errCh := make(chan error, 1)
	go func() {
		httpLabel, wsLabel := "HTTP", "WS"
		if tlsEnabled {
			httpLabel, wsLabel = "HTTPS", "WSS"
		}
		log.Printf("%s/%s listening on %s control=%s audio=%s (serving embedded bridge UI)", httpLabel, wsLabel, cfg.Listen, controlWSPath, audioWSPath)
		if tlsEnabled {
			errCh <- srv.ListenAndServeTLS(cfg.TLS.Cert, cfg.TLS.Key)
		} else {
			errCh <- srv.ListenAndServe()
		}
	}()

	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		var lastRx, lastTx, lastRB, lastTB uint64
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				rx := atomic.LoadUint64(&rxFrames)
				tx := atomic.LoadUint64(&txFrames)
				rb := atomic.LoadUint64(&rxBytes)
				tb := atomic.LoadUint64(&txBytes)
				pf := atomic.LoadUint64(&prtpControlFrames)
				pe := atomic.LoadUint64(&prtpCRCErrors)
				rsg := atomic.LoadUint64(&rxSeqGapEvents)
				rr := atomic.LoadUint64(&rxReordered)
				rsm := atomic.LoadUint64(&rxSeqMissing)
				rs := atomic.LoadUint64(&rxStale)
				rfr := atomic.LoadUint64(&rxFragmentsRecovered)
				rfd := atomic.LoadUint64(&rxFragmentsDropped)
				drx := rx - lastRx
				dtx := tx - lastTx
				drb := rb - lastRB
				dtb := tb - lastTB
				lastRx, lastTx, lastRB, lastTB = rx, tx, rb, tb
				log.Printf("gateway: rx=%d (+%d/2s, %dB) tx=%d (+%d/2s, %dB)", rx, drx, drb, tx, dtx, dtb)
				h.broadcastJSON(map[string]any{
					"type":              "stats",
					"rx_frames":         rx,
					"tx_frames":         tx,
					"rx_fps":            float64(drx) / 2,
					"tx_fps":            float64(dtx) / 2,
					"rx_bytes":          rb,
					"tx_bytes":          tb,
					"prtp_frames":       pf,
					"prtp_crc_errors":   pe,
					"rx_seq_gap_events": rsg,
					"rx_out_of_order":   rr,
					"rx_reordered":      rr,
					"rx_seq_missing":    rsm,
					"rx_stale":          rs,
					"rx_frag_recovered": rfr,
					"rx_frag_dropped":   rfd,
				})
			}
		}
	}()

	select {
	case <-ctx.Done():
		_ = udpConn.Close()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		if err := <-errCh; err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}

type wsAudioInputState struct {
	pcmBuf    []int16
	inputRate int
	resampler *audio.Int16StreamResampler
	fir       *audio.FIRFilter
}

type client struct {
	conn      *websocket.Conn
	control   bool
	audio     bool
	textSend  chan []byte
	audioSend chan []byte
	done      chan struct{}
	closeOnce sync.Once
}

func newClient(conn *websocket.Conn, control, audio bool) *client {
	return &client{
		conn:      conn,
		control:   control,
		audio:     audio,
		textSend:  make(chan []byte, wsTextQueueMessages),
		audioSend: make(chan []byte, wsAudioQueueFrames),
		done:      make(chan struct{}),
	}
}

func (c *client) close() {
	if c == nil {
		return
	}
	c.closeOnce.Do(func() {
		close(c.done)
		if c.conn != nil {
			_ = c.conn.Close()
		}
	})
}

func (c *client) writeJSON(v any) {
	msg, err := json.Marshal(v)
	if err != nil {
		return
	}
	c.writeText(msg)
}

func (c *client) writeText(data []byte) {
	if c == nil || c.conn == nil {
		return
	}
	msg := cloneBytes(data)
	select {
	case <-c.done:
		return
	default:
	}
	select {
	case c.textSend <- msg:
	case <-c.done:
	default:
		c.close()
	}
}

func (c *client) writeBinary(bin []byte) {
	if c == nil || c.conn == nil {
		return
	}
	msg := cloneBytes(bin)
	select {
	case <-c.done:
		return
	default:
	}
	select {
	case c.audioSend <- msg:
		return
	case <-c.done:
		return
	default:
	}
	select {
	case <-c.audioSend:
	default:
	}
	select {
	case c.audioSend <- msg:
	case <-c.done:
	default:
	}
}

func (c *client) writeLoop() {
	for {
		select {
		case <-c.done:
			return
		default:
		}
		select {
		case msg := <-c.textSend:
			if !c.writeRaw(websocket.TextMessage, msg) {
				c.close()
				return
			}
			continue
		default:
		}
		select {
		case msg := <-c.textSend:
			if !c.writeRaw(websocket.TextMessage, msg) {
				c.close()
				return
			}
		case msg := <-c.audioSend:
			if !c.writeRaw(websocket.BinaryMessage, msg) {
				c.close()
				return
			}
		case <-c.done:
			return
		}
	}
}

func (c *client) writeRaw(messageType int, data []byte) bool {
	if c == nil || c.conn == nil {
		return false
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	return c.conn.WriteMessage(messageType, data) == nil
}

type hub struct {
	mu       sync.Mutex
	clients  map[*client]struct{}
	snapshot []cachedBroadcast
}

type cachedBroadcast struct {
	key string
	msg []byte
}

func newHub() *hub { return &hub{clients: make(map[*client]struct{})} }

func (h *hub) add(c *client) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

func (h *hub) remove(c *client) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
	c.close()
}

func (h *hub) broadcast(bin []byte) {
	h.mu.Lock()
	clients := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		if c.audio {
			clients = append(clients, c)
		}
	}
	h.mu.Unlock()
	for _, c := range clients {
		c.writeBinary(bin)
	}
}

func (h *hub) broadcastJSON(v any) {
	msg, _ := json.Marshal(v)
	h.mu.Lock()
	h.cacheSnapshotLocked(v, msg)
	clients := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		if c.control {
			clients = append(clients, c)
		}
	}
	h.mu.Unlock()
	for _, c := range clients {
		c.writeText(msg)
	}
}

func (h *hub) replaySnapshot(c *client) {
	h.mu.Lock()
	msgs := make([][]byte, 0, len(h.snapshot))
	for _, item := range h.snapshot {
		msg := make([]byte, len(item.msg))
		copy(msg, item.msg)
		msgs = append(msgs, msg)
	}
	h.mu.Unlock()
	for _, msg := range msgs {
		c.writeText(msg)
	}
}

func (h *hub) hasSnapshotKey(key string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, item := range h.snapshot {
		if item.key == key {
			return true
		}
	}
	return false
}

func (h *hub) cacheSnapshotLocked(v any, msg []byte) {
	key := snapshotKey(v)
	if key == "" {
		return
	}
	if key == "prtp_info:keys" {
		h.snapshot = removeCachedSnapshotPrefix(h.snapshot, "prtp_info:key_event:")
	}
	copied := make([]byte, len(msg))
	copy(copied, msg)
	for i := range h.snapshot {
		if h.snapshot[i].key == key {
			h.snapshot[i].msg = copied
			return
		}
	}
	h.snapshot = append(h.snapshot, cachedBroadcast{key: key, msg: copied})
}

func removeCachedSnapshotPrefix(items []cachedBroadcast, prefix string) []cachedBroadcast {
	out := items[:0]
	for _, item := range items {
		if strings.HasPrefix(item.key, prefix) {
			continue
		}
		out = append(out, item)
	}
	return out
}

func snapshotKey(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	t, _ := m["type"].(string)
	switch t {
	case "prtp_emulation", "prtp_simulation":
		return "prtp_emulation"
	case "matrix_names":
		if okValue, ok := m["ok"].(bool); ok && okValue {
			return "matrix_names"
		}
		return ""
	case "prtp_info":
		kind, _ := m["kind"].(string)
		switch kind {
		case "ident", "keys":
			return "prtp_info:" + kind
		case "key_event", "label":
			return "prtp_info:" + kind + ":" + fmt.Sprint(m["index"])
		case "leds":
			return "prtp_info:leds:" + fmt.Sprint(m["mode"])
		}
	}
	return ""
}

func noCacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, max-age=0")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		next.ServeHTTP(w, r)
	})
}

func announceEmulationOnce(h *hub, p *prtp.EmulationProfile, state *prtp.EmulationState, sendPRTP func([]byte), reason string, debug bool) {
	if p == nil || state == nil || sendPRTP == nil || !state.AnnounceOnce() {
		return
	}
	log.Printf("PRTP emulation announcing %s %q ident=%q (%d keys, type=0x%02X, user=%s, fpga=%s, reason=%s)", p.Model, p.Name, prtp.EmulationIdentityCode(p), p.KeyCount, p.TypeCode, prtp.FormatUserVersion(p.UserVersion), prtp.FormatFPGAVersion(p.FPGAVersion), reason)
	for _, payload := range prtp.EmulationPayloads(p) {
		frame := prtp.BuildFrame(payload)
		if debug {
			log.Printf("PRTP emulation outbound payload=[%s] frame=[%s]", prtp.HexBytes(payload), prtp.HexBytes(frame))
		}
		sendPRTP(frame)
	}
	publishEmulationSnapshot(p, h.broadcastJSON)
}

func publishEmulationSnapshot(p *prtp.EmulationProfile, writeJSON func(any)) {
	if p == nil || writeJSON == nil {
		return
	}
	writeJSON(map[string]any{
		"type":         "prtp_emulation",
		"model":        p.Model,
		"kind":         p.Kind,
		"name":         p.Name,
		"type_code":    int(p.TypeCode),
		"keys":         p.KeyCount,
		"user_version": prtp.FormatUserVersion(p.UserVersion),
		"fpga_version": prtp.FormatFPGAVersion(p.FPGAVersion),
	})
	writeJSON(map[string]any{"type": "prtp_info", "kind": "ident", "text": p.Name})
	groups := make([]int, prtp.BitmapGroupCount(p.KeyCount))
	writeJSON(map[string]any{"type": "prtp_info", "kind": "keys", "count": len(groups), "groups": groups})
	for _, mode := range []string{"g_fix", "r_fix", "g_blink", "r_blink"} {
		writeJSON(map[string]any{"type": "prtp_info", "kind": "leds", "mode": mode, "count": len(groups), "groups": groups})
	}
	for i, label := range p.Labels {
		writeJSON(map[string]any{"type": "prtp_info", "kind": "label", "index": i, "text": label})
	}
}

func emitInfo(h *hub, payload []byte, sendPRTP func([]byte), sendPRTPNormal func([]byte), sentSync *bool, emu *prtp.EmulationProfile, debug bool) {
	if len(payload) == 0 {
		return
	}
	t0 := payload[0]
	switch t0 {
	case 0x50:
		h.broadcastJSON(map[string]any{"type": "prtp_info", "kind": "ping"})
		if sendPRTP != nil {
			sendPRTP(prtp.BuildFrame([]byte{0x41}))
			if sentSync != nil && !*sentSync && emulationStateSyncEnabled(emu) {
				sendPRTP(prtp.BuildFrame([]byte{0x53}))
				*sentSync = true
			}
		}
		return
	case 0x41:
		h.broadcastJSON(map[string]any{"type": "prtp_info", "kind": "ack"})
		return
	case 0x4E:
		h.broadcastJSON(map[string]any{"type": "prtp_info", "kind": "nack"})
		return
	case 0x52:
		state := 0
		if len(payload) >= 2 {
			state = int(payload[1])
		}
		h.broadcastJSON(map[string]any{"type": "prtp_info", "kind": "r", "state": state})
		if sendPRTP != nil {
			sendPRTP(prtp.BuildFrame([]byte{0x41}))
		}
		return
	case 0x43:
		h.broadcastJSON(map[string]any{"type": "prtp_info", "kind": "cmd"})
		if sendPRTP != nil {
			sendPRTP(prtp.BuildFrame([]byte{0x41}))
		}
		return
	}
	if t0 != 0x49 || len(payload) < 2 {
		h.broadcastJSON(map[string]any{"type": "prtp_info", "kind": "unknown", "t0": int(t0)})
		return
	}
	if sendPRTP != nil {
		sendPRTP(prtp.BuildFrame([]byte{0x41}))
	}
	if len(payload) >= 3 && payload[1] == 0x00 {
		key := payload[2]
		h.broadcastJSON(map[string]any{
			"type":    "prtp_info",
			"kind":    "key_event",
			"index":   int(key & 0x7F),
			"pressed": (key & 0x80) != 0,
		})
		return
	}
	sub := payload[1] & 0xF0
	cnt := int(payload[1] & 0x0F)
	switch sub {
	case 0x40:
		groups := make([]int, 0, cnt)
		for j := 0; j < cnt && 2+j < len(payload); j++ {
			groups = append(groups, int(payload[2+j]))
		}
		h.broadcastJSON(map[string]any{"type": "prtp_info", "kind": "keys", "count": cnt, "groups": groups})
	case 0x50, 0x60, 0x70, 0x80:
		mode := map[byte]string{0x50: "g_fix", 0x60: "r_fix", 0x70: "g_blink", 0x80: "r_blink"}[sub]
		groups := make([]int, 0, cnt)
		for j := 0; j < cnt && 2+j < len(payload); j++ {
			groups = append(groups, int(payload[2+j]))
		}
		h.broadcastJSON(map[string]any{"type": "prtp_info", "kind": "leds", "mode": mode, "count": cnt, "groups": groups})
	case 0x90:
		if len(payload) >= 3 {
			idx := int(payload[2])
			ln := cnt - 1
			if ln < 0 {
				ln = 0
			}
			start := 3
			end := start + ln
			if end > len(payload) {
				end = len(payload)
			}
			txt := string(payload[start:end])
			h.broadcastJSON(map[string]any{"type": "prtp_info", "kind": "label", "index": idx, "text": txt})
		}
	case 0xD0:
		txt := prtp.ParseIdentityText(payload)
		if strings.TrimSpace(txt) != "" {
			h.broadcastJSON(map[string]any{"type": "prtp_info", "kind": "ident", "text": txt})
		}
		if emu != nil && sendPRTPNormal != nil {
			response := prtp.IdentityPayload(emu)
			frame := prtp.BuildFrame(response)
			if debug {
				log.Printf("PRTP emulation identity response payload=[%s] frame=[%s]", prtp.HexBytes(response), prtp.HexBytes(frame))
			}
			sendPRTPNormal(frame)
		}
	default:
		h.broadcastJSON(map[string]any{"type": "prtp_info", "kind": "i_unknown", "sub": int(sub)})
	}
}

func emulationStateSyncEnabled(emu *prtp.EmulationProfile) bool {
	return emu == nil || emu.TypeCode != prtp.TypeBP7100
}

func emulationDeviceAuto(device string) bool {
	token := strings.ToLower(strings.TrimSpace(device))
	token = strings.NewReplacer("-", "", "_", "", " ", "").Replace(token)
	return token == "auto" || token == "autodetect"
}

func emulationProfileFromMatrixTarget(target matrix.PortName, cfg config.EmulationConfig) (*prtp.EmulationProfile, bool, error) {
	device, ok := prtp.EmulationDeviceForTypeCode(target.TypeCode)
	if !ok {
		return nil, false, nil
	}
	name := strings.TrimSpace(cfg.Name)
	if name == "" {
		name = strings.ToUpper(device)
		if port := strings.TrimSpace(target.Port); port != "" {
			name += "-" + port
		}
	}
	profile, err := prtp.NewEmulationProfile(device, name, cfg.Keys)
	if err != nil {
		return nil, false, err
	}
	if err := prtp.ApplyEmulationVersions(profile, cfg.UserVersion, cfg.FPGAVersion); err != nil {
		return nil, false, err
	}
	return profile, true, nil
}

func matrixNamesMessage(snap *matrix.NameSnapshot) map[string]any {
	if snap == nil {
		return map[string]any{"type": "matrix_names", "ok": false, "error": "empty helper response"}
	}
	return map[string]any{
		"type":          "matrix_names",
		"ok":            true,
		"addr":          snap.Addr,
		"device":        snap.Device,
		"target":        snap.Target,
		"ports":         snap.Ports,
		"button_labels": snap.ButtonLabels,
		"map_labels":    snap.MapLabels,
		"map_bank":      snap.MapBank,
		"map_bank_name": snap.MapBankName,
		"map_size":      snap.MapSize,
		"map_error":     snap.MapError,
		"strings":       snap.Strings,
	}
}

func matrixAddrFromMessage(m map[string]any, fallback string) string {
	if s, _ := m["addr"].(string); strings.TrimSpace(s) != "" {
		return matrix.NormalizeAddr(s)
	}
	if s, _ := m["host"].(string); strings.TrimSpace(s) != "" {
		return matrix.NormalizeAddr(s)
	}
	return matrix.NormalizeAddr(fallback)
}

func tlsEnabled(tlsCfg config.TLSConfig) (bool, error) {
	certFile := strings.TrimSpace(tlsCfg.Cert)
	keyFile := strings.TrimSpace(tlsCfg.Key)
	if certFile == "" && keyFile == "" {
		return false, nil
	}
	if certFile == "" || keyFile == "" {
		return false, errors.New("tls.cert and tls.key must be provided together")
	}
	return true, nil
}

func cloneBytes(in []byte) []byte {
	if len(in) == 0 {
		return nil
	}
	out := make([]byte, len(in))
	copy(out, in)
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func jsonMapInt(m map[string]any, key string) (int, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch x := v.(type) {
	case float64:
		return int(x), true
	case int:
		return x, true
	case string:
		var n int
		_, err := fmt.Sscanf(strings.TrimSpace(x), "%d", &n)
		return n, err == nil
	default:
		return 0, false
	}
}

func jsonMapString(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok {
		return "", false
	}
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x), true
	default:
		return strings.TrimSpace(fmt.Sprint(x)), true
	}
}

func jsonMapBool(m map[string]any, key string) (bool, bool) {
	v, ok := m[key]
	if !ok {
		return false, false
	}
	switch x := v.(type) {
	case bool:
		return x, true
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "true", "1", "yes", "on":
			return true, true
		case "false", "0", "no", "off":
			return false, true
		default:
			return false, false
		}
	default:
		return false, false
	}
}

func ternary[T any](cond bool, a, b T) T {
	if cond {
		return a
	}
	return b
}
