# PRTP Bridge RX Audio Follow-Ups

This tracks future RX audio-quality work for `prtp-bridge`.

## Current Finding

- The browser UI RX VU meter is fed from samples leaving the adaptive AudioWorklet and is delayed by the browser-reported output timing when available. It therefore follows rendered browser audio rather than decoded packet arrival.
- Browser RX starts at about 92 ms for network audio and 61 ms for frontend loopback, catches up at up to 1.03x, removes verified leading silence, and enforces a 350 ms hard queue ceiling.
- The current RX playback resamplers are still linear interpolators, and measured response for 8333 Hz to playback-rate conversion shows high-frequency droop. This remains an audible-quality concern independent of the rendered VU timing.
- Custom G.711 decode is memoryless and flat for steady sine roundtrips in the tested range. The NET3 TX map behaves like a constant gain shift, not a frequency-shaped filter.

## TODO

- Add an RX measurement mode that reports raw decoded PCM RMS, peak, and windowed RMS separately from the smoothed UI meter.
- Make the VU meter use a longer or configurable RMS window for sweep tests, optionally with a Hann window, so swept tones do not look like level instability.
- Add a dump-analysis script for `matrix_rx_pcm` / `matrix_rx_g711` captures that can generate per-frequency level plots from NET3 test sweeps.
- Replace browser RX playback linear interpolation with a higher-quality streaming resampler if listening tests show audible high-frequency loss.
- Replace Go server-playback linear interpolation with the same higher-quality resampler so browser and server RX paths have comparable frequency response.
- Add automated resampler frequency-response tests for 8333 Hz input to 16 kHz and 48 kHz output, with a passband ripple target suitable for intercom audio.
- Keep TX anti-alias filtering and TX map changes separate from RX VU work unless capture analysis proves the oscillation is already present before the bridge receives RX PCM.
