//go:build (darwin || windows || linux) && cgo

package audio

import (
	"reflect"
	"testing"
	"unsafe"
)

func TestLocalPlaybackCallbackCopiesMonoToEveryOutputChannel(t *testing.T) {
	la := &malgoLocal{
		playbackEnabled: true,
		sampleRate:      16000,
		frameSamples:    4,
		playbackChans:   2,
		playbackQueue:   make(chan []int16, LocalAudioQueueFrames),
	}
	la.Play([]int16{100, -200, 300, -400})

	out := make([]int16, 8)
	la.playbackCallback(int16Bytes(out), nil, 4)

	want := []int16{100, 100, -200, -200, 300, 300, -400, -400}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("playback output = %v, want %v", out, want)
	}
}

func TestLocalPlaybackCallbackFillsUnderrunWithSilence(t *testing.T) {
	la := &malgoLocal{
		playbackEnabled: true,
		sampleRate:      16000,
		frameSamples:    4,
		playbackChans:   2,
		playbackQueue:   make(chan []int16, LocalAudioQueueFrames),
	}
	la.Play([]int16{11, 22})

	out := []int16{-1, -1, -1, -1, -1, -1, -1, -1}
	la.playbackCallback(int16Bytes(out), nil, 4)

	want := []int16{11, 11, 22, 22, 0, 0, 0, 0}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("playback underrun output = %v, want %v", out, want)
	}
}

func TestLocalCaptureCallbackAveragesInterleavedChannels(t *testing.T) {
	la := &malgoLocal{
		captureEnabled: true,
		sampleRate:     16000,
		frameSamples:   4,
		captureChans:   2,
		captureChan:    make(chan []int16, LocalAudioQueueFrames),
	}

	in := []int16{100, 300, -300, 100, 32767, 32767, -32768, -32768}
	la.captureCallback(nil, int16Bytes(in), 4)

	got := <-la.RecordChan()
	want := []int16{200, -100, 32767, -32768}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("captured mono frame = %v, want %v", got, want)
	}
}

func TestLocalDuplexCallbackMovesPlaybackAndCaptureInOneAudioCycle(t *testing.T) {
	la := &malgoLocal{
		playbackEnabled: true,
		captureEnabled:  true,
		sampleRate:      16000,
		frameSamples:    3,
		playbackChans:   2,
		captureChans:    2,
		playbackQueue:   make(chan []int16, LocalAudioQueueFrames),
		captureChan:     make(chan []int16, LocalAudioQueueFrames),
	}
	la.Play([]int16{7, 8, 9})

	out := make([]int16, 6)
	in := []int16{10, 30, 20, 40, -10, -30}
	la.duplexCallback(int16Bytes(out), int16Bytes(in), 3)

	wantOut := []int16{7, 7, 8, 8, 9, 9}
	if !reflect.DeepEqual(out, wantOut) {
		t.Fatalf("duplex playback output = %v, want %v", out, wantOut)
	}
	gotCapture := <-la.RecordChan()
	wantCapture := []int16{20, 30, -20}
	if !reflect.DeepEqual(gotCapture, wantCapture) {
		t.Fatalf("duplex capture frame = %v, want %v", gotCapture, wantCapture)
	}
}

func int16Bytes(samples []int16) []byte {
	if len(samples) == 0 {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(&samples[0])), len(samples)*2)
}
