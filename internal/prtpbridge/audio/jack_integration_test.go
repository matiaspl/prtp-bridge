//go:build linux && cgo

package audio

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestJACKNamedDuplexClientLoopbackFlowAndQuality(t *testing.T) {
	if os.Getenv("KROMA_JACK_INTEGRATION") != "1" {
		t.Skip("set KROMA_JACK_INTEGRATION=1 to run the live JACK integration test")
	}
	cleanupJack := ensureJACKServer(t)
	defer cleanupJack()

	sampleRate := currentJACKSampleRate(t)
	frameSamples := 128
	clientName := NormalizeClientName(fmt.Sprintf("%s-test-%d", DefaultClientName, time.Now().UnixNano()%1_000_000))

	local, err := NewLocalWithOptions(LocalOptions{
		Playback:     true,
		Capture:      true,
		SampleRate:   sampleRate,
		FrameSamples: frameSamples,
		ClientName:   clientName,
	})
	if err != nil {
		t.Fatalf("start JACK local audio client %q: %v", clientName, err)
	}
	defer local.Close()

	outputs, inputs := waitForJACKDuplexPorts(t, clientName)
	t.Logf("JACK client %s outputs=%v inputs=%v", clientName, outputs, inputs)
	for i, input := range inputs {
		output := outputs[i%len(outputs)]
		jackConnect(t, output, input)
		defer jackDisconnect(output, input)
	}

	reference := deterministicAudioPattern(frameSamples * 32)
	captured := runJACKLoopback(t, local, reference, sampleRate, frameSamples)
	corr, gain, nrms := bestNormalizedMatch(reference[:frameSamples*12], captured)
	t.Logf("JACK loopback quality: corr=%.4f gain=%.3f nrms=%.4f captured=%d", corr, gain, nrms, len(captured))
	if corr < 0.98 {
		t.Fatalf("JACK loopback correlation = %.4f, want >= 0.98 (gain %.3f, nrms %.4f)", corr, gain, nrms)
	}
	if gain < 0.90 || gain > 1.10 {
		t.Fatalf("JACK loopback gain = %.3f, want 0.90..1.10 (corr %.4f, nrms %.4f)", gain, corr, nrms)
	}
	if nrms > 0.08 {
		t.Fatalf("JACK loopback normalized RMS error = %.4f, want <= 0.08 (corr %.4f, gain %.3f)", nrms, corr, gain)
	}
}

func ensureJACKServer(t *testing.T) func() {
	t.Helper()
	if jackLSP() == nil {
		return func() {}
	}
	if os.Getenv("KROMA_JACK_START_DUMMY") != "1" {
		t.Fatalf("JACK server is not reachable; start JACK or set KROMA_JACK_START_DUMMY=1")
	}
	if _, err := exec.LookPath("jackd"); err != nil {
		t.Fatalf("jackd not found for KROMA_JACK_START_DUMMY=1: %v", err)
	}
	cmd := exec.Command("jackd", "-d", "dummy", "-r", "48000", "-p", "256")
	var logBuf bytes.Buffer
	cmd.Stdout = &logBuf
	cmd.Stderr = &logBuf
	cmd.Env = append(os.Environ(), "JACK_NO_AUDIO_RESERVATION=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start dummy JACK server: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if jackLSP() == nil {
			return func() {
				_ = cmd.Process.Kill()
				_, _ = cmd.Process.Wait()
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	t.Fatalf("dummy JACK server did not become ready:\n%s", logBuf.String())
	return func() {}
}

func jackLSP(args ...string) error {
	cmdArgs := append([]string{}, args...)
	cmd := exec.Command("jack_lsp", cmdArgs...)
	return cmd.Run()
}

func currentJACKSampleRate(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("jack_samplerate").Output()
	if err != nil {
		t.Fatalf("jack_samplerate: %v", err)
	}
	rate, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || rate <= 0 {
		t.Fatalf("invalid JACK sample rate %q", strings.TrimSpace(string(out)))
	}
	return rate
}

type jackPortInfo struct {
	name   string
	input  bool
	output bool
}

func waitForJACKDuplexPorts(t *testing.T, clientName string) (outputs, inputs []string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var ports []jackPortInfo
	for time.Now().Before(deadline) {
		ports = jackPorts(t, clientName)
		outputs, inputs = splitJACKPorts(ports)
		if len(outputs) > 0 && len(inputs) > 0 {
			return outputs, inputs
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("JACK client %q did not expose both source and sink ports; ports=%+v", clientName, ports)
	return nil, nil
}

func jackPorts(t *testing.T, clientName string) []jackPortInfo {
	t.Helper()
	out, err := exec.Command("jack_lsp", "-p").Output()
	if err != nil {
		t.Fatalf("jack_lsp -p: %v", err)
	}
	var ports []jackPortInfo
	current := -1
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			current = -1
			name := strings.TrimSpace(line)
			if strings.HasPrefix(name, clientName+":") {
				ports = append(ports, jackPortInfo{name: name})
				current = len(ports) - 1
			}
			continue
		}
		if current < 0 {
			continue
		}
		props := strings.ToLower(line)
		if strings.Contains(props, "output") {
			ports[current].output = true
		}
		if strings.Contains(props, "input") {
			ports[current].input = true
		}
	}
	return ports
}

func splitJACKPorts(ports []jackPortInfo) (outputs, inputs []string) {
	for _, port := range ports {
		if port.output {
			outputs = append(outputs, port.name)
		}
		if port.input {
			inputs = append(inputs, port.name)
		}
	}
	sort.Strings(outputs)
	sort.Strings(inputs)
	return outputs, inputs
}

func jackConnect(t *testing.T, output, input string) {
	t.Helper()
	out, err := exec.Command("jack_connect", output, input).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "already exists") {
		t.Fatalf("jack_connect %q %q: %v\n%s", output, input, err, string(out))
	}
}

func jackDisconnect(output, input string) {
	_ = exec.Command("jack_disconnect", output, input).Run()
}

func deterministicAudioPattern(n int) []int16 {
	r := rand.New(rand.NewSource(0x711))
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(r.Intn(24001) - 12000)
	}
	return out
}

func runJACKLoopback(t *testing.T, local Local, reference []int16, sampleRate, frameSamples int) []int16 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	frameDur := time.Duration(frameSamples) * time.Second / time.Duration(sampleRate)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(frameDur)
		defer ticker.Stop()
		pos := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				block := make([]int16, frameSamples)
				for i := range block {
					block[i] = reference[pos%len(reference)]
					pos++
				}
				local.Play(block)
			}
		}
	}()

	var captured []int16
	deadline := time.After(1500 * time.Millisecond)
	for {
		select {
		case frame, ok := <-local.RecordChan():
			if !ok {
				t.Fatal("local capture channel closed")
			}
			captured = append(captured, frame...)
			if len(captured) >= len(reference)*2 {
				cancel()
				<-done
				return captured
			}
		case <-deadline:
			cancel()
			<-done
			if len(captured) < len(reference)/2 {
				t.Fatalf("captured only %d samples from JACK loopback", len(captured))
			}
			return captured
		}
	}
}

func bestNormalizedMatch(reference, captured []int16) (corr, gain, nrms float64) {
	if len(reference) == 0 || len(captured) < len(reference) {
		return 0, 0, 1
	}
	ref := int16ToFloat64(reference)
	refEnergy := dot(ref, ref)
	bestCorr := -1.0
	bestGain := 0.0
	bestErr := 1.0
	for off := 0; off <= len(captured)-len(reference); off++ {
		seg := int16ToFloat64(captured[off : off+len(reference)])
		segEnergy := dot(seg, seg)
		if segEnergy <= 0 {
			continue
		}
		c := dot(ref, seg) / math.Sqrt(refEnergy*segEnergy)
		if c <= bestCorr {
			continue
		}
		g := dot(ref, seg) / refEnergy
		errEnergy := 0.0
		for i := range ref {
			d := seg[i] - g*ref[i]
			errEnergy += d * d
		}
		bestCorr = c
		bestGain = g
		bestErr = math.Sqrt(errEnergy / refEnergy)
	}
	return bestCorr, bestGain, bestErr
}

func int16ToFloat64(in []int16) []float64 {
	out := make([]float64, len(in))
	for i, v := range in {
		out[i] = float64(v)
	}
	return out
}

func dot(a, b []float64) float64 {
	sum := 0.0
	for i := range a {
		sum += a[i] * b[i]
	}
	return sum
}
