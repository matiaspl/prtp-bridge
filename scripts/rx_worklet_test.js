'use strict';

const assert = require('node:assert/strict');
const test = require('node:test');
const {
  desiredPlaybackRate,
  DEFAULT_SILENCE_THRESHOLD,
  KromaRxJitterBuffer,
} = require('../internal/prtpbridge/webui/rx-worklet.js');

test('occupancy controller stays within the allowed rate range', () => {
  const packet = 1475;
  const target = packet * 3;
  assert.equal(desiredPlaybackRate(target, target, packet), 1);
  assert.equal(desiredPlaybackRate(target - packet, target, packet), 0.99875);
  assert.equal(desiredPlaybackRate(target + packet / 2, target, packet), 1);
  assert.ok(desiredPlaybackRate(target + packet * 2, target, packet) > 1);
  assert.equal(desiredPlaybackRate(target + packet * 10, target, packet), 1.03);
});

test('continuous catch-up removes a 150 ms backlog within six seconds', () => {
  const sampleRate = 48000;
  const packet = sampleRate * 256 / 8333;
  const target = packet * 3;
  let buffered = target + sampleRate * 0.15;
  for (let output = 0; output < sampleRate * 6; output += 128) {
    const rate = desiredPlaybackRate(buffered, target, packet);
    buffered += 128 - 128 * rate;
  }
  assert.ok(buffered / sampleRate < 0.110, `buffer remained at ${buffered / sampleRate}s`);
});

test('leading silence is trimmed but speech is preserved', () => {
  const processor = new KromaRxJitterBuffer();
  processor.targetFrames = 4800;
  processor.packetFrames = 1475;
  processor.minSilenceFrames = 960;
  processor.enqueue(new Float32Array(9000));
  const trimmed = processor.trimLeadingSilence();
  assert.ok(trimmed >= 960);
  assert.equal(processor.silenceTrimmedFrames, Math.floor(trimmed));

  processor.reset();
  processor.targetFrames = 4800;
  processor.packetFrames = 1475;
  processor.minSilenceFrames = 960;
  const speech = new Float32Array(9000);
  speech.fill(DEFAULT_SILENCE_THRESHOLD * 2);
  processor.enqueue(speech);
  assert.equal(processor.trimLeadingSilence(), 0);
  assert.equal(processor.count, speech.length);
});

test('hard ceiling performs one bounded crossfaded trim', () => {
  const processor = new KromaRxJitterBuffer();
  processor.targetFrames = 4400;
  processor.maxFrames = 12000;
  processor.hardMaxFrames = 16800;
  const signal = new Float32Array(24000);
  for (let i = 0; i < signal.length; i++) signal[i] = Math.sin(i / 10) * 0.2;
  processor.enqueue(signal);
  const trimmed = processor.enforceHardCeiling();
  assert.ok(trimmed > 0);
  assert.ok(processor.count <= processor.targetFrames + 2);
  assert.ok(processor.crossfade);
  assert.equal(processor.emergencyTrimmedFrames, Math.floor(trimmed));
});

test('prefill silence produces zero-level rendered meter windows', () => {
  const processor = new KromaRxJitterBuffer();
  const messages = [];
  processor.port = { postMessage: (message) => messages.push(message) };
  processor.targetFrames = 4800;
  for (let i = 0; i < 20; i++) {
    processor.process([], [[new Float32Array(128)]]);
  }
  const meters = messages.filter((message) => message.type === 'meter');
  assert.ok(meters.length > 0, 'silence did not produce a rendered meter window');
  assert.equal(meters.at(-1).rms, 0);
  assert.equal(meters.at(-1).peak, 0);
});
