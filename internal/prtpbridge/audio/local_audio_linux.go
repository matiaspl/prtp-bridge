//go:build linux && cgo

package audio

import "github.com/gen2brain/malgo"

func localAudioBackends() []malgo.Backend  { return []malgo.Backend{malgo.BackendJack} }
func BackendName() string                  { return "jack" }
func localAudioPrefersDuplex() bool        { return true }
func localAudioUsesSyntheticDevices() bool { return true }
func localAudioClientChannels() uint32     { return 2 }
