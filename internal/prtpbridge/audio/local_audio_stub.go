//go:build !((darwin || windows || linux) && cgo)

package audio

import (
	"errors"
	"strings"
)

const DefaultDeviceID = "default"

func Supported() bool { return false }

func NewLocal(playback, capture bool, sampleRate, frameSamples int) (Local, error) {
	return nil, errors.New("local audio backend not supported on this platform")
}

func NewLocalWithDevices(playback, capture bool, sampleRate, frameSamples int, playbackID, captureID string) (Local, error) {
	return nil, errors.New("local audio backend not supported on this platform")
}

func NewLocalWithOptions(opts LocalOptions) (Local, error) {
	return nil, errors.New("local audio backend not supported on this platform")
}

func NormalizeDeviceID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" || strings.EqualFold(id, DefaultDeviceID) {
		return DefaultDeviceID
	}
	return id
}

func ListDevices() (DeviceSnapshot, error) {
	return DeviceSnapshot{Supported: false, Backend: "unsupported"}, nil
}

func BackendName() string { return "unsupported" }
