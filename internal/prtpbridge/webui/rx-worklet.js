/*
 * Low-latency mono RX jitter buffer for the embedded Web UI.
 * Keep the policy helpers side-effect free so they can also be exercised by
 * scripts/rx_worklet_test.js without a browser or an npm dependency.
 */
(function (scope) {
  'use strict';

  const DEFAULT_SILENCE_THRESHOLD = Math.pow(10, -48 / 20);

  function desiredPlaybackRate(bufferedFrames, targetFrames, packetFrames) {
    const packet = Math.max(1, packetFrames || 1);
    const errorPackets = (bufferedFrames - targetFrames) / packet;
    if (errorPackets < -0.5) {
      return Math.max(0.995, 1 + (errorPackets + 0.5) * 0.0025);
    }
    if (errorPackets <= 0.5) return 1;
    if (errorPackets < 1) {
      return 1 + ((errorPackets - 0.5) / 0.5) * 0.03;
    }
    return 1.03;
  }

  function isSilentSample(value, threshold) {
    return Math.abs(value || 0) <= (threshold || DEFAULT_SILENCE_THRESHOLD);
  }

  const WorkletBase = typeof scope.AudioWorkletProcessor === 'function'
    ? scope.AudioWorkletProcessor
    : class {};

  class KromaRxJitterBuffer extends WorkletBase {
    constructor() {
      super();
      const rate = typeof sampleRate === 'number' ? sampleRate : 48000;
      this.sampleRate = rate;
      this.capacity = Math.max(16384, Math.ceil(rate * 3));
      this.buffer = new Float32Array(this.capacity);
      this.read = 0;
      this.write = 0;
      this.count = 0;
      this.packetFrames = Math.round(rate * 256 / 8333);
      this.targetFrames = this.packetFrames * 3;
      this.maxFrames = Math.round(rate * 0.25);
      this.hardMaxFrames = Math.round(rate * 0.35);
      this.silenceThreshold = DEFAULT_SILENCE_THRESHOLD;
      this.minSilenceFrames = Math.max(1, Math.round(rate * 0.02));
      this.crossfadeFrames = Math.max(1, Math.round(rate * 0.004));
      this.started = false;
      this.playbackRate = 1;
      this.underruns = 0;
      this.droppedFrames = 0;
      this.silenceTrimmedFrames = 0;
      this.emergencyTrimmedFrames = 0;
      this.reportCountdown = 0;
      this.meterCountdown = Math.max(1, Math.round(rate * 0.05));
      this.meterSum = 0;
      this.meterPeak = 0;
      this.meterFrames = 0;
      this.crossfade = null;
      if (this.port) this.port.onmessage = (ev) => this.handleMessage(ev.data || {});
    }

    handleMessage(msg) {
      if (msg.type === 'config') {
        if (Number.isFinite(msg.packetFrames)) {
          this.packetFrames = Math.max(128, Math.min(this.capacity, Math.floor(msg.packetFrames)));
        }
        if (Number.isFinite(msg.targetFrames)) {
          this.targetFrames = Math.max(128, Math.min(this.capacity, Math.floor(msg.targetFrames)));
        }
        if (Number.isFinite(msg.maxFrames)) {
          this.maxFrames = Math.max(this.targetFrames, Math.min(this.capacity, Math.floor(msg.maxFrames)));
        }
        if (Number.isFinite(msg.hardMaxFrames)) {
          this.hardMaxFrames = Math.max(this.maxFrames, Math.min(this.capacity, Math.floor(msg.hardMaxFrames)));
        }
        if (Number.isFinite(msg.silenceThreshold) && msg.silenceThreshold > 0) {
          this.silenceThreshold = msg.silenceThreshold;
        }
      } else if (msg.type === 'audio' && msg.data && msg.data.length) {
        this.enqueue(msg.data);
      } else if (msg.type === 'reset') {
        this.reset();
      }
    }

    reset() {
      this.read = 0;
      this.write = 0;
      this.count = 0;
      this.started = false;
      this.playbackRate = 1;
      this.crossfade = null;
      this.underruns = 0;
      this.droppedFrames = 0;
      this.silenceTrimmedFrames = 0;
      this.emergencyTrimmedFrames = 0;
      this.meterSum = 0;
      this.meterPeak = 0;
      this.meterFrames = 0;
      this.meterCountdown = Math.max(1, Math.round(this.sampleRate * 0.05));
      this.report();
    }

    sampleAt(offset) {
      const pos = this.read + offset;
      const base = Math.floor(pos);
      const frac = pos - base;
      const a = this.buffer[((base % this.capacity) + this.capacity) % this.capacity] || 0;
      const b = this.buffer[(((base + 1) % this.capacity) + this.capacity) % this.capacity] || 0;
      return a + (b - a) * frac;
    }

    advance(frames) {
      const n = Math.max(0, Math.min(this.count, frames));
      this.read = (this.read + n) % this.capacity;
      this.count -= n;
      if (this.count < 0.0001) this.count = 0;
      return n;
    }

    beginCrossfadedDrop(frames) {
      const n = Math.max(0, Math.min(this.count - 2, Math.floor(frames)));
      if (!n) return 0;
      const old = new Float32Array(this.crossfadeFrames);
      for (let i = 0; i < old.length; i++) old[i] = this.sampleAt(i);
      this.advance(n);
      this.crossfade = { old, index: 0 };
      return n;
    }

    enqueue(input) {
      let data = input;
      if (data.length > this.capacity) {
        this.droppedFrames += data.length - this.capacity;
        data = data.subarray(data.length - this.capacity);
      }
      const overflow = this.count + data.length - this.capacity;
      if (overflow > 0) {
        this.advance(overflow);
        this.droppedFrames += Math.floor(overflow);
      }
      let offset = 0;
      while (offset < data.length) {
        const take = Math.min(data.length - offset, this.capacity - this.write);
        this.buffer.set(data.subarray(offset, offset + take), this.write);
        this.write = (this.write + take) % this.capacity;
        this.count += take;
        offset += take;
      }
    }

    trimLeadingSilence() {
      const excess = Math.floor(this.count - this.targetFrames);
      if (excess < this.minSilenceFrames) return 0;
      const scan = Math.min(excess, this.packetFrames * 4);
      let silent = 0;
      while (silent < scan && isSilentSample(this.sampleAt(silent), this.silenceThreshold)) silent++;
      if (silent < this.minSilenceFrames) return 0;
      const leave = Math.min(this.crossfadeFrames, silent);
      const dropped = this.advance(silent - leave);
      this.silenceTrimmedFrames += Math.floor(dropped);
      this.droppedFrames += Math.floor(dropped);
      return dropped;
    }

    enforceHardCeiling() {
      if (this.count <= this.hardMaxFrames) return 0;
      const drop = this.count - this.targetFrames;
      const dropped = this.beginCrossfadedDrop(drop);
      this.emergencyTrimmedFrames += Math.floor(dropped);
      this.droppedFrames += Math.floor(dropped);
      return dropped;
    }

    report() {
      if (!this.port) return;
      this.port.postMessage({
        type: 'stats',
        bufferedFrames: Math.max(0, Math.floor(this.count)),
        targetFrames: this.targetFrames,
        playbackRate: this.playbackRate,
        underruns: this.underruns,
        droppedFrames: this.droppedFrames,
        silenceTrimmedFrames: this.silenceTrimmedFrames,
        emergencyTrimmedFrames: this.emergencyTrimmedFrames,
        active: this.started,
      });
    }

    reportMeter(renderedFrame) {
      if (!this.port || !this.meterFrames) return;
      this.port.postMessage({
        type: 'meter',
        rms: Math.sqrt(this.meterSum / this.meterFrames),
        peak: this.meterPeak,
        renderedFrame,
      });
      this.meterSum = 0;
      this.meterPeak = 0;
      this.meterFrames = 0;
    }

    recordMeterSample(value, renderedFrame) {
      const magnitude = Math.abs(value);
      this.meterSum += value * value;
      this.meterPeak = Math.max(this.meterPeak, magnitude);
      this.meterFrames++;
      this.meterCountdown--;
      if (this.meterCountdown <= 0) {
        this.reportMeter(renderedFrame);
        this.meterCountdown = Math.max(1, Math.round(this.sampleRate * 0.05));
      }
    }

    process(_, outputs) {
      const out = outputs[0] && outputs[0][0];
      if (!out) return true;
      if (!this.started && this.count < this.targetFrames) {
        out.fill(0);
        const frameBase = typeof currentFrame === 'number' ? currentFrame : 0;
        for (let i = 0; i < out.length; i++) this.recordMeterSample(0, frameBase + i);
        this.reportCountdown -= out.length;
        if (this.reportCountdown <= 0) {
          this.reportCountdown = Math.floor(this.sampleRate / 4);
          this.report();
        }
        return true;
      }
      this.started = true;
      this.enforceHardCeiling();
      this.trimLeadingSilence();

      const wantedRate = desiredPlaybackRate(this.count, this.targetFrames, this.packetFrames);
      const smoothing = 1 - Math.exp(-out.length / (this.sampleRate * 0.12));
      this.playbackRate += (wantedRate - this.playbackRate) * smoothing;
      this.playbackRate = Math.max(0.995, Math.min(1.03, this.playbackRate));

      let produced = 0;
      for (; produced < out.length; produced++) {
        if (this.count < this.playbackRate + 1) break;
        let value = this.sampleAt(0);
        if (this.crossfade) {
          const i = this.crossfade.index;
          const mix = Math.min(1, (i + 1) / this.crossfade.old.length);
          value = this.crossfade.old[i] * (1 - mix) + value * mix;
          this.crossfade.index++;
          if (this.crossfade.index >= this.crossfade.old.length) this.crossfade = null;
        }
        out[produced] = value;
        this.advance(this.playbackRate);
        const frame = typeof currentFrame === 'number' ? currentFrame + produced : 0;
        this.recordMeterSample(value, frame);
      }
      if (produced < out.length) {
        out.fill(0, produced);
        const frameBase = typeof currentFrame === 'number' ? currentFrame : 0;
        for (let i = produced; i < out.length; i++) this.recordMeterSample(0, frameBase + i);
        this.read = this.write;
        this.count = 0;
        this.started = false;
        this.playbackRate = 1;
        this.underruns++;
        if (this.port) this.port.postMessage({ type: 'underrun', underruns: this.underruns });
      }
      this.reportCountdown -= out.length;
      if (this.reportCountdown <= 0) {
        this.reportCountdown = Math.floor(this.sampleRate / 4);
        this.report();
      }
      return true;
    }
  }

  const helpers = {
    desiredPlaybackRate,
    isSilentSample,
    DEFAULT_SILENCE_THRESHOLD,
    KromaRxJitterBuffer,
  };
  if (typeof module !== 'undefined' && module.exports) module.exports = helpers;
  scope.KromaRxWorkletPolicy = helpers;
  if (typeof scope.registerProcessor === 'function') {
    scope.registerProcessor('kroma-rx-jitter-buffer', KromaRxJitterBuffer);
  }
})(typeof globalThis !== 'undefined' ? globalThis : this);
