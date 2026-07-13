//go:build windows && cgo

package audio

import "github.com/gen2brain/malgo"

func localAudioBackends() []malgo.Backend  { return []malgo.Backend{malgo.BackendWasapi} }
func BackendName() string                  { return "wasapi" }
func localAudioPrefersDuplex() bool        { return false }
func localAudioUsesSyntheticDevices() bool { return false }
func localAudioClientChannels() uint32     { return 1 }
