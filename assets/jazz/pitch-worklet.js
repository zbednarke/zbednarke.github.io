"use strict";

// Jazz Project effects engine: autocorrelation pitch detection + dual-tap granular pitch shifter.
// Modes: off (dry passthrough, tuner still runs), tune (autotune), harmony (diatonic voices).

class PitchEngine extends AudioWorkletProcessor {
  constructor() {
    super();
    this.W = Math.round(sampleRate * 0.035); // shifter grain window (samples)
    this.rb = new Float32Array(32768);
    this.mask = this.rb.length - 1;
    this.w = 0;
    this.det = new Float32Array(2048);
    this.sinceDetect = 0;
    this.msgTick = 0;

    this.mode = "off";
    this.keyRoot = 0;
    this.scale = [0, 2, 4, 5, 7, 9, 11];
    this.strength = 0.9;
    this.glideMs = 40;
    this.voicing = "third";
    this.harmMix = 0.7;

    this.f0 = 0;
    this.midi = 0;
    this.rms = 0;
    this.tuneRatio = 1; this.tuneTarget = 1;
    this.vR = [1, 1]; this.vT = [1, 1]; this.vOn = [0, 0];
    this.ph = [0, 0, 0];

    this.port.onmessage = (e) => Object.assign(this, e.data);
  }

  read(delaySamples) {
    const pos = this.w - 1 - delaySamples;
    const i0 = Math.floor(pos);
    const frac = pos - i0;
    const a = this.rb[i0 & this.mask];
    const b = this.rb[(i0 + 1) & this.mask];
    return a + (b - a) * frac;
  }

  tapVoice(k, ratio) {
    this.ph[k] += (1 - ratio) / this.W;
    this.ph[k] -= Math.floor(this.ph[k]);
    const p1 = this.ph[k];
    const p2 = (p1 + 0.5) % 1;
    return this.read(p1 * this.W) * Math.sin(Math.PI * p1) +
           this.read(p2 * this.W) * Math.sin(Math.PI * p2);
  }

  corrAt(buf, M, lag) {
    let s = 0;
    for (let i = 0; i < M; i += 2) s += buf[i] * buf[i + lag];
    return s;
  }

  detect() {
    const N = this.det.length, buf = this.det;
    for (let i = 0; i < N; i++) buf[i] = this.rb[(this.w - N + i) & this.mask];
    let rms = 0;
    for (let i = 0; i < N; i++) rms += buf[i] * buf[i];
    rms = Math.sqrt(rms / N);
    this.rms = rms;
    if (rms < 0.008) { this.f0 = 0; this.tuneTarget = 1; return; }

    const minLag = Math.max(2, Math.floor(sampleRate / 1000));
    const maxLag = Math.min(N - 2, Math.floor(sampleRate / 70));
    const M = N - maxLag;
    let best = -Infinity, bestLag = -1;
    for (let lag = minLag; lag <= maxLag; lag++) {
      const s = this.corrAt(buf, M, lag);
      if (s > best) { best = s; bestLag = lag; }
    }
    if (bestLag < 0) { this.f0 = 0; return; }
    const half = Math.round(bestLag / 2);
    if (half >= minLag && this.corrAt(buf, M, half) > 0.9 * best) bestLag = half;

    const c0 = this.corrAt(buf, M, bestLag - 1);
    const c1 = this.corrAt(buf, M, bestLag);
    const c2 = this.corrAt(buf, M, bestLag + 1);
    const den = c0 - 2 * c1 + c2;
    let shift = 0;
    if (Math.abs(den) > 1e-9) shift = Math.max(-1, Math.min(1, 0.5 * (c0 - c2) / den));
    this.f0 = sampleRate / (bestLag + shift);
    this.midi = 69 + 12 * Math.log2(this.f0 / 440);

    if (this.mode === "tune") {
      const snap = this.snapToScale(this.midi);
      this.tuneTarget = Math.pow(2, ((snap - this.midi) * this.strength) / 12);
    } else {
      this.tuneTarget = 1;
    }

    if (this.mode === "harmony") {
      const pc = ((Math.round(this.midi) - this.keyRoot) % 12 + 12) % 12;
      const deg = this.degreeOf(pc);
      let s0 = 0, s1 = 0, on1 = 0;
      if (this.voicing === "third") s0 = this.stepsUp(deg, 2);
      else if (this.voicing === "triad") { s0 = this.stepsUp(deg, 2); s1 = this.stepsUp(deg, 4); on1 = 1; }
      else if (this.voicing === "fifthdown") s0 = this.stepsUp(deg, 4) - 12;
      else if (this.voicing === "octaves") { s0 = -12; s1 = 12; on1 = 1; }
      this.vT[0] = Math.pow(2, s0 / 12);
      this.vT[1] = Math.pow(2, s1 / 12);
      this.vOn[0] = 1; this.vOn[1] = on1;
    }
  }

  snapToScale(m) {
    const r = Math.round(m);
    let best = r, bestD = 99;
    for (let c = r - 6; c <= r + 6; c++) {
      const pc = ((c - this.keyRoot) % 12 + 12) % 12;
      if (this.scale.includes(pc)) {
        const d = Math.abs(c - m);
        if (d < bestD) { bestD = d; best = c; }
      }
    }
    return best;
  }

  degreeOf(pc) {
    let bi = 0, bd = 99;
    for (let i = 0; i < this.scale.length; i++) {
      const s = this.scale[i];
      const d = Math.min(((pc - s) % 12 + 12) % 12, ((s - pc) % 12 + 12) % 12);
      if (d < bd) { bd = d; bi = i; }
    }
    return bi;
  }

  stepsUp(deg, steps) {
    const L = this.scale.length;
    const idx = deg + steps;
    const oct = Math.floor(idx / L);
    return this.scale[idx % L] + 12 * oct - this.scale[deg];
  }

  process(inputs, outputs) {
    const inp = inputs[0] && inputs[0][0];
    const out = outputs[0][0];
    if (!inp || !out) return true;
    const k = Math.exp(-1 / ((Math.max(1, this.glideMs) / 1000) * sampleRate));
    for (let i = 0; i < inp.length; i++) {
      this.rb[this.w & this.mask] = inp[i];
      this.w++;
      this.tuneRatio = this.tuneRatio * k + this.tuneTarget * (1 - k);
      this.vR[0] = this.vR[0] * k + this.vT[0] * (1 - k);
      this.vR[1] = this.vR[1] * k + this.vT[1] * (1 - k);
      const dry = inp[i];
      if (this.mode === "tune") {
        out[i] = this.tapVoice(0, this.tuneRatio);
      } else if (this.mode === "harmony") {
        out[i] = dry + this.harmMix *
          (this.vOn[0] * this.tapVoice(1, this.vR[0]) +
           this.vOn[1] * this.tapVoice(2, this.vR[1]));
      } else {
        out[i] = dry;
      }
    }
    this.sinceDetect += inp.length;
    if (this.sinceDetect >= 1024) {
      this.sinceDetect = 0;
      this.detect();
      if (++this.msgTick % 2 === 0) {
        this.port.postMessage({ f0: this.f0, midi: this.f0 ? this.midi : 0, rms: this.rms });
      }
    }
    return true;
  }
}

registerProcessor("pitch-engine", PitchEngine);
