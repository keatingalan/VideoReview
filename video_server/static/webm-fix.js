/**
 * webm-fix.js
 *
 * Complete post-processing for Chrome MediaRecorder WebM output.
 *
 * Problems fixed (in a single linear pass + one append):
 *
 *  1. AUDIO PTS DRIFT      – Chrome derives audio timestamps from wall-clock
 *                            time; rewritten from cumulative sample count
 *                            (mirrors ffmpeg asetpts=N/SR/TB).
 *
 *  2. MISSING DURATION     – Info.Duration is absent; computed from the last
 *                            block timestamp + one frame and written back into
 *                            the Info element in-place.
 *
 *  3. NOT SEEKABLE / NO CUES – No Cues element means players cannot seek.
 *                            One CuePoint per cluster (pointing at the video
 *                            keyframe that starts each cluster) is appended
 *                            after all clusters.
 *
 *  4. MISSING / WRONG SeekHead – Chrome either omits SeekHead or writes one
 *                            that only points at Info/Tracks. A new SeekHead
 *                            is written at the very start of the Segment
 *                            pointing at Info, Tracks, and Cues.
 *
 *  5. UNKNOWN SEGMENT SIZE  – Chrome writes 0x01FFFFFFFFFFFF; replaced with
 *                            the actual byte count so the file is
 *                            self-describing.
 *
 *  6. NON-MONOTONIC TIMESTAMPS – Gaps or backward jumps in cluster timestamps
 *                            caused by wall-clock recording are detected and
 *                            corrected so timestamps are strictly increasing.
 *
 * Everything is lossless — no bitstream is decoded or re-encoded.
 *
 * Output layout:
 *   EBML header  (copied verbatim)
 *   Segment      (known size)
 *     SeekHead   (new — points to Info, Tracks, Cues)
 *     Void       (padding so cluster offsets stay valid after SeekHead grows)
 *     Info       (Duration patched in-place)
 *     Tracks     (copied verbatim)
 *     Cluster…   (audio PTS rewritten, cluster timestamps monotonised)
 *     Cues       (new — appended after last cluster)
 *
 * Usage:
 *   import { fixWebM } from './webm-fix.js';
 *   const fixedBlob = await fixWebM(rawBlob);
 *
 * Or as a drop-in MediaRecorder wrapper:
 *   import { WebMRecorder } from './webm-fix.js';
 *   const rec = new WebMRecorder(stream);
 *   rec.start();
 *   rec.stop();
 *   const blob = await rec.getFixedBlob();
 */

// ─────────────────────────────────────────────────────────────────────────────
// EBML element IDs
// ─────────────────────────────────────────────────────────────────────────────

const ID = Object.freeze({
  EBML:              0x1A45DFA3,
  SEGMENT:           0x18538067,
  SEEK_HEAD:         0x114D9B74,
  SEEK:              0x4DBB,
  SEEK_ID:           0x53AB,
  SEEK_POSITION:     0x53AC,
  VOID:              0xEC,
  INFO:              0x1549A966,
  TIMESTAMP_SCALE:   0x2AD7B1,
  DURATION:          0x4489,
  MUXING_APP:        0x4D80,
  WRITING_APP:       0x5741,
  TRACKS:            0x1654AE6B,
  TRACK_ENTRY:       0xAE,
  TRACK_NUMBER:      0xD7,
  TRACK_TYPE:        0x83,
  AUDIO:             0xE1,
  SAMPLING_FREQ:     0xB5,
  CLUSTER:           0x1F43B675,
  CLUSTER_TIMESTAMP: 0xE7,
  PREV_SIZE:         0xAB,
  SIMPLE_BLOCK:      0xA3,
  BLOCK_GROUP:       0xA0,
  BLOCK:             0xA1,
  CUES:              0x1C53BB6B,
  CUE_POINT:         0xBB,
  CUE_TIME:          0xB3,
  CUE_TRACK_POS:     0xB7,
  CUE_TRACK:         0xF7,
  CUE_CLUSTER_POS:   0xF1,
});

const TRACK_TYPE_AUDIO = 2;
const TRACK_TYPE_VIDEO = 1;
const UNKNOWN_SIZE     = 0x00FF_FFFF_FFFF_FFFF;

// SeekHead is pre-allocated with a fixed size (Void-padded) so cluster byte
// offsets don't need to be recomputed if the SeekHead grows slightly.
// 3 Seek entries × ~20 bytes each + SeekHead header = ~80 bytes.
// We reserve 100 bytes of body, padded with a Void element.
const SEEKHEAD_RESERVED_BODY = 100;

// ─────────────────────────────────────────────────────────────────────────────
// Low-level EBML read/write helpers
// ─────────────────────────────────────────────────────────────────────────────

function readID(buf, pos) {
  const b = buf[pos];
  let width = 1, mask = 0x80;
  while ((b & mask) === 0) { width++; mask >>= 1; }
  let id = 0;
  for (let i = 0; i < width; i++) id = (id * 256 + buf[pos + i]) >>> 0;
  return { id, width };
}

function readVint(buf, pos) {
  const b = buf[pos];
  let width = 1, mask = 0x80;
  while ((b & mask) === 0) { width++; mask >>= 1; }
  let value = b & ~mask;
  for (let i = 1; i < width; i++) value = value * 256 + buf[pos + i];
  if (value === Math.pow(2, 7 * width) - 1) value = UNKNOWN_SIZE;
  return { value, width };
}

function readUint(buf, pos, n) {
  let v = 0;
  for (let i = 0; i < n; i++) v = v * 256 + buf[pos + i];
  return v;
}

function readInt16(buf, pos) {
  const v = buf[pos] * 256 + buf[pos + 1];
  return v >= 0x8000 ? v - 0x10000 : v;
}

function readFloat(view, pos, n) {
  return n === 4 ? view.getFloat32(pos, false) : view.getFloat64(pos, false);
}

/** Encode data-size VINT (marker bit set). */
function encodeVint(v) {
  if (v < 0x7F)       return new Uint8Array([v | 0x80]);
  if (v < 0x3FFF)     return new Uint8Array([(v >> 8) | 0x40, v & 0xFF]);
  if (v < 0x1FFFFF)   return new Uint8Array([(v >> 16) | 0x20, (v >> 8) & 0xFF, v & 0xFF]);
  if (v < 0x0FFFFFFF) return new Uint8Array([(v >> 24) | 0x10, (v >> 16) & 0xFF, (v >> 8) & 0xFF, v & 0xFF]);
  // 5-byte for values up to ~34 GB
  const hi = Math.floor(v / 0x1_0000_0000), lo = v >>> 0;
  return new Uint8Array([((hi & 0x07) | 0x08), (lo >>> 24) & 0xFF, (lo >>> 16) & 0xFF, (lo >>> 8) & 0xFF, lo & 0xFF]);
}

/** Encode element ID as raw bytes (marker bit is part of the ID). */
function encodeID(id) {
  if (id <= 0xFF)     return new Uint8Array([id]);
  if (id <= 0xFFFF)   return new Uint8Array([id >> 8, id & 0xFF]);
  if (id <= 0xFFFFFF) return new Uint8Array([id >> 16, (id >> 8) & 0xFF, id & 0xFF]);
  return new Uint8Array([(id >>> 24) & 0xFF, (id >>> 16) & 0xFF, (id >> 8) & 0xFF, id & 0xFF]);
}

/** Encode v as a fixed-width big-endian unsigned integer (n bytes). */
function encodeUintFixed(v, n) {
  const out = new Uint8Array(n);
  for (let i = n - 1; i >= 0; i--) { out[i] = v & 0xFF; v = Math.floor(v / 256); }
  return out;
}

/** Encode v as a fixed-width big-endian signed int16. */
function encodeInt16(v) {
  const u = v < 0 ? v + 0x10000 : v;
  return new Uint8Array([u >> 8, u & 0xFF]);
}

/** Encode v as an IEEE 754 float64 big-endian (8 bytes). */
function encodeFloat64(v) {
  const buf = new ArrayBuffer(8);
  new DataView(buf).setFloat64(0, v, false);
  return new Uint8Array(buf);
}

// ─────────────────────────────────────────────────────────────────────────────
// Growable output buffer
// ─────────────────────────────────────────────────────────────────────────────

class Chunks {
  constructor() { this.parts = []; this.byteLength = 0; }

  push(arr) {
    if (arr.byteLength > 0) { this.parts.push(arr); this.byteLength += arr.byteLength; }
  }

  /** Append a complete EBML element. */
  pushElement(id, body) {
    this.push(encodeID(id));
    this.push(encodeVint(body.byteLength ?? body.length));
    this.push(body instanceof Uint8Array ? body : body.toUint8Array());
  }

  toUint8Array() {
    const out = new Uint8Array(this.byteLength);
    let off = 0;
    for (const p of this.parts) { out.set(p, off); off += p.byteLength; }
    return out;
  }
}

// ─────────────────────────────────────────────────────────────────────────────
// First pass: scan — collect metadata needed before writing
// ─────────────────────────────────────────────────────────────────────────────

/**
 * ScanResult holds everything discovered in the first pass.
 */
class ScanResult {
  constructor() {
    this.timestampScale  = 1_000_000;   // ns per tick
    this.tracks          = new Map();   // num → {number,isAudio,isVideo,sampleRate}
    this.clusterTimestamps = [];         // absolute TS of each cluster (ticks)
    this.lastBlockAbsTS  = 0;           // last seen absolute block TS
    this.hasVideo        = false;
    this.hasDuration     = false;
    // byte offset of the Info body start within the Segment body
    // (used so we can patch Duration in the second pass)
    this.infoDurationOffset = -1;       // offset inside Info body where Duration sits
    this.infoDurationWidth  = 0;        // existing float width (4 or 8), or 0 = must insert
  }
}

function scan(buf) {
  const result  = new ScanResult();
  const view    = new DataView(buf.buffer, buf.byteOffset, buf.byteLength);
  let pos       = 0;

  // Skip EBML header
  const ebmlID = readID(buf, pos); pos += ebmlID.width;
  const ebmlSz = readVint(buf, pos); pos += ebmlSz.width + ebmlSz.value;

  // Enter Segment
  const segID  = readID(buf, pos); pos += segID.width;
  const segSz  = readVint(buf, pos); pos += segSz.width;
  // segBody start = pos

  const segEnd = segSz.value === UNKNOWN_SIZE ? buf.length : pos + segSz.value;

  while (pos < segEnd) {
    let idR, szR;
    try { idR = readID(buf, pos); } catch { break; }
    pos += idR.width;
    try { szR = readVint(buf, pos); } catch { break; }
    pos += szR.width;

    const bodyStart = pos;
    const bodyEnd   = szR.value === UNKNOWN_SIZE ? segEnd : Math.min(pos + szR.value, buf.length);

    switch (idR.id) {
      case ID.INFO:
        scanInfo(buf, view, bodyStart, bodyEnd - bodyStart, result);
        break;
      case ID.TRACKS:
        scanTracks(buf, view, bodyStart, bodyEnd - bodyStart, result);
        break;
      case ID.CLUSTER:
        scanCluster(buf, bodyStart, bodyEnd - bodyStart, result, bodyStart);
        break;
    }

    pos = szR.value === UNKNOWN_SIZE ? bodyEnd : pos + szR.value;
    if (pos > buf.length) break;
  }

  return result;
}

function scanInfo(buf, view, start, size, result) {
  let i = 0;
  while (i < size) {
    let idR, szR;
    try { idR = readID(buf, start + i); i += idR.width; } catch { break; }
    try { szR = readVint(buf, start + i); i += szR.width; } catch { break; }
    if (idR.id === ID.TIMESTAMP_SCALE) {
      result.timestampScale = readUint(buf, start + i, szR.value);
    } else if (idR.id === ID.DURATION) {
      result.hasDuration = true;
    }
    i += szR.value;
  }
}

function scanTracks(buf, view, start, size, result) {
  let i = 0;
  while (i < size) {
    let idR, szR;
    try { idR = readID(buf, start + i); i += idR.width; } catch { break; }
    try { szR = readVint(buf, start + i); i += szR.width; } catch { break; }
    if (idR.id === ID.TRACK_ENTRY) {
      scanTrackEntry(buf, view, start + i, szR.value, result);
    }
    i += szR.value;
  }
}

function scanTrackEntry(buf, view, start, size, result) {
  const t = { number: 0, isAudio: false, isVideo: false, sampleRate: 48000 };
  let i = 0;
  while (i < size) {
    let idR, szR;
    try { idR = readID(buf, start + i); i += idR.width; } catch { break; }
    try { szR = readVint(buf, start + i); i += szR.width; } catch { break; }
    switch (idR.id) {
      case ID.TRACK_NUMBER: t.number  = readUint(buf, start + i, szR.value); break;
      case ID.TRACK_TYPE:   {
        const tt = readUint(buf, start + i, szR.value);
        t.isAudio = tt === TRACK_TYPE_AUDIO;
        t.isVideo = tt === TRACK_TYPE_VIDEO;
        break;
      }
      case ID.AUDIO:
        t.sampleRate = scanAudioEl(buf, view, start + i, szR.value);
        break;
    }
    i += szR.value;
  }
  if (t.number > 0) {
    result.tracks.set(t.number, t);
    if (t.isVideo) result.hasVideo = true;
  }
}

function scanAudioEl(buf, view, start, size) {
  let i = 0;
  while (i < size) {
    let idR, szR;
    try { idR = readID(buf, start + i); i += idR.width; } catch { break; }
    try { szR = readVint(buf, start + i); i += szR.width; } catch { break; }
    if (idR.id === ID.SAMPLING_FREQ) {
      const absOff = buf.byteOffset + start + i;
      return new DataView(buf.buffer).getFloat32(absOff, false);
    }
    i += szR.value;
  }
  return 48000;
}

function scanCluster(buf, start, size, result, absStart) {
  let clusterTS = 0;
  let i = 0;
  const end = Math.min(size, buf.length - start);
  while (i < end) {
    let idR, szR;
    try { idR = readID(buf, start + i); i += idR.width; } catch { break; }
    try { szR = readVint(buf, start + i); i += szR.width; } catch { break; }
    if (szR.value !== UNKNOWN_SIZE && start + i + szR.value > buf.length) break;

    if (idR.id === ID.CLUSTER_TIMESTAMP) {
      clusterTS = readUint(buf, start + i, szR.value);
      result.clusterTimestamps.push(clusterTS);
    } else if (idR.id === ID.SIMPLE_BLOCK || idR.id === ID.BLOCK) {
      // Track the highest absolute timestamp seen
      if (szR.value >= 3) {
        const tnR    = readVint(buf, start + i);
        const relTS  = readInt16(buf, start + i + tnR.width);
        const absTS  = clusterTS + relTS;
        if (absTS > result.lastBlockAbsTS) result.lastBlockAbsTS = absTS;
      }
    }
    if (szR.value === UNKNOWN_SIZE) break;
    i += szR.value;
  }
}

// ─────────────────────────────────────────────────────────────────────────────
// Second pass: rewrite
// ─────────────────────────────────────────────────────────────────────────────

class Rewriter {
  constructor(buf, scanResult) {
    this.buf          = buf;
    this.view         = new DataView(buf.buffer, buf.byteOffset, buf.byteLength);
    this.sr           = scanResult;
    this.out          = new Chunks();   // collects the full output

    // Per-track state for audio PTS rewriting
    this.audioSamples = new Map();      // trackNum → cumulative sample count

    // Cues collected during cluster rewriting
    this.cuePoints    = [];             // [{cueTime, cueTrack, clusterOffset}]

    // Running byte offset within the Segment body (for Cue cluster positions)
    this.segBodyOffset = 0;

    // Monotonic cluster timestamp enforcer
    this.prevClusterTS = -1;
  }

  rewrite() {
    let pos = 0;

    // ── 1. Copy EBML header verbatim ──────────────────────────────────────────
    const ebmlID = readID(this.buf, pos); pos += ebmlID.width;
    const ebmlSz = readVint(this.buf, pos); pos += ebmlSz.width;
    this.out.push(encodeID(ID.EBML));
    this.out.push(encodeVint(ebmlSz.value));
    this.out.push(this.buf.subarray(pos, pos + ebmlSz.value));
    pos += ebmlSz.value;

    // ── 2. Open Segment (we'll patch the size at the end) ────────────────────
    const segIDStart = pos;
    const segIDBytes = encodeID(ID.SEGMENT);
    const segSzBytes = new Uint8Array(8); // placeholder — 8-byte VINT
    pos += readID(this.buf, pos).width;
    pos += readVint(this.buf, pos).width;

    // We'll go back and fill in the real size.  Reserve placeholders now.
    const segSizePlaceholderIndex = this.out.parts.length + 1; // index after push
    this.out.push(segIDBytes);
    const segSzChunkIndex = this.out.parts.length;
    this.out.push(segSzBytes); // placeholder

    const segBodyStart = this.out.byteLength;

    // ── 3. SeekHead placeholder (fixed size, Void-padded) ────────────────────
    //    We write the real SeekHead after collecting Cues offsets,
    //    but we must reserve its exact space now so cluster offsets are stable.
    const seekHeadChunkIndex = this.out.parts.length;
    const seekHeadPlaceholder = new Uint8Array(
      encodeID(ID.SEEK_HEAD).length + encodeVint(SEEKHEAD_RESERVED_BODY).length + SEEKHEAD_RESERVED_BODY
    );
    this.out.push(seekHeadPlaceholder);
    this.segBodyOffset += seekHeadPlaceholder.byteLength;

    // ── 4. Parse and emit Info, Tracks, Clusters ─────────────────────────────
    const segEnd = pos + (
      readVint(this.buf, segIDStart + readID(this.buf, segIDStart).width).value === UNKNOWN_SIZE
        ? this.buf.length - segIDStart - readID(this.buf, segIDStart).width - 8
        : readVint(this.buf, segIDStart + readID(this.buf, segIDStart).width).value
    );

    // Re-enter Segment body
    while (pos < this.buf.length) {
      let idR, szR;
      try { idR = readID(this.buf, pos); } catch { break; }
      pos += idR.width;
      try { szR = readVint(this.buf, pos); } catch { break; }
      pos += szR.width;

      const bodyStart = pos;
      const isUnknown = szR.value === UNKNOWN_SIZE;
      const bodyEnd   = isUnknown ? this.buf.length : Math.min(pos + szR.value, this.buf.length);

      switch (idR.id) {
        case ID.SEEK_HEAD:
          // Skip the old SeekHead — we replace it entirely.
          pos = bodyEnd;
          continue;
        case ID.VOID:
          // Drop old Void padding.
          pos = bodyEnd;
          continue;
        case ID.INFO:
          this.emitInfo(bodyStart, bodyEnd - bodyStart);
          break;
        case ID.TRACKS:
          this.emitVerbatim(ID.TRACKS, bodyStart, bodyEnd - bodyStart);
          break;
        case ID.CLUSTER:
          this.emitCluster(bodyStart, bodyEnd - bodyStart, isUnknown);
          break;
        case ID.CUES:
          // Drop old (possibly empty) Cues — we regenerate them.
          pos = bodyEnd;
          continue;
        default:
          this.emitVerbatim(idR.id, bodyStart, bodyEnd - bodyStart);
          break;
      }

      pos = bodyEnd;
      if (isUnknown || pos >= this.buf.length) break;
    }

    // ── 5. Append Cues ───────────────────────────────────────────────────────
    const cuesOffset = this.out.byteLength - segBodyStart;
    const cuesBody   = this.buildCues();
    this.out.pushElement(ID.CUES, cuesBody);

    // ── 6. Build and splice in the real SeekHead ──────────────────────────────
    const infoOffset   = this._infoOffset   - segBodyStart;
    const tracksOffset = this._tracksOffset - segBodyStart;
    const seekHead     = this.buildSeekHead(infoOffset, tracksOffset, cuesOffset);
    // Splice into placeholder slot
    this.out.parts[seekHeadChunkIndex] = seekHead;
    this.out.byteLength -= seekHeadPlaceholder.byteLength;
    this.out.byteLength += seekHead.byteLength;

    // ── 7. Patch Segment size ─────────────────────────────────────────────────
    const segBodyBytes = this.out.byteLength - segBodyStart;
    const realSegSize  = encode8ByteVint(segBodyBytes);
    this.out.parts[segSzChunkIndex] = realSegSize;
    // byteLength doesn't change since placeholder was already 8 bytes

    return new Blob([this.out.toUint8Array()], { type: 'video/webm' });
  }

  // ── Info: patch/insert Duration, rewrite MuxingApp/WritingApp ──────────────

  emitInfo(bodyStart, bodySize) {
    this._infoOffset = this.out.byteLength;

    const src = this.buf.subarray(bodyStart, bodyStart + bodySize);

    // Compute duration: last block TS + one default frame, in ticks.
    const lastTS       = this.sr.lastBlockAbsTS;
    const oneTick      = defaultFrameTicks(this.sr);
    const durationTick = lastTS + oneTick;   // in timestampScale ticks

    // Rebuild Info body, inserting/replacing Duration and patching writing app.
    const info = new Chunks();
    let i = 0;
    let hasDuration = false;
    while (i < src.length) {
      let idR, szR;
      try { idR = readID(src, i); i += idR.width; } catch { break; }
      try { szR = readVint(src, i); i += szR.width; } catch { break; }
      switch (idR.id) {
        case ID.DURATION:
          // Replace with corrected value (float64, 8 bytes).
          info.pushElement(ID.DURATION, encodeFloat64(durationTick));
          hasDuration = true;
          break;
        case ID.WRITING_APP:
          info.pushElement(ID.WRITING_APP, new TextEncoder().encode('webm-fix.js'));
          break;
        default:
          info.pushElement(idR.id, src.subarray(i, i + szR.value));
          break;
      }
      i += szR.value;
    }
    if (!hasDuration) {
      // Chrome omits Duration entirely — insert it.
      info.pushElement(ID.DURATION, encodeFloat64(durationTick));
    }

    this.out.pushElement(ID.INFO, info.toUint8Array());
    this.segBodyOffset += encodeID(ID.INFO).length + encodeVint(info.byteLength).length + info.byteLength;
  }

  // ── Tracks: copy verbatim ──────────────────────────────────────────────────

  emitVerbatim(id, bodyStart, bodySize) {
    if (id === ID.TRACKS) this._tracksOffset = this.out.byteLength;
    const body = this.buf.subarray(bodyStart, bodyStart + bodySize);
    this.out.pushElement(id, body);
    this.segBodyOffset += encodeID(id).length + encodeVint(bodySize).length + bodySize;
  }

  // ── Cluster: fix audio PTS, monotonise timestamps, collect Cue points ──────

  emitCluster(bodyStart, bodySize, isUnknown) {
    const clusterSegOffset = this.out.byteLength - this._seekHeadSize();

    const src = this.buf.subarray(bodyStart, bodyStart + (isUnknown ? this.buf.length - bodyStart : bodySize));
    const clusterBody = new Chunks();

    let i            = 0;
    let rawClusterTS = 0;
    let fixedClusterTS = 0;
    let firstVideoKeyframe = true;

    while (i < src.length) {
      let idR, szR;
      try { idR = readID(src, i); i += idR.width; } catch { break; }
      try { szR = readVint(src, i); i += szR.width; } catch { break; }
      if (szR.value !== UNKNOWN_SIZE && i + szR.value > src.length) break;

      const bStart = i;

      switch (idR.id) {
        case ID.CLUSTER_TIMESTAMP: {
          rawClusterTS = readUint(src, i, szR.value);
          // Monotonicity fix: ensure cluster TS is >= previous cluster TS.
          if (rawClusterTS <= this.prevClusterTS && this.prevClusterTS >= 0) {
            fixedClusterTS = this.prevClusterTS + 1;
          } else {
            fixedClusterTS = rawClusterTS;
          }
          this.prevClusterTS = fixedClusterTS;
          clusterBody.pushElement(ID.CLUSTER_TIMESTAMP, encodeUintFixed(fixedClusterTS, szR.value));
          break;
        }
        case ID.SIMPLE_BLOCK: {
          const raw     = src.subarray(i, i + szR.value);
          const shifted = fixedClusterTS - rawClusterTS; // adjustment for monotonicity
          const rewritten = this.rewriteBlock(raw, rawClusterTS, fixedClusterTS);
          // Collect Cue point for first video keyframe in cluster
          if (firstVideoKeyframe && isVideoKeyframe(src, i, szR.value, this.sr)) {
            this.cuePoints.push({
              cueTime:       fixedClusterTS,
              cueTrack:      getTrackNum(src, i),
              clusterOffset: this.out.byteLength - (this._infoOffset - (this.out.byteLength - this.segBodyOffset)),
            });
            firstVideoKeyframe = false;
          }
          clusterBody.pushElement(ID.SIMPLE_BLOCK, rewritten);
          break;
        }
        case ID.BLOCK_GROUP: {
          const raw       = src.subarray(i, i + szR.value);
          const rewritten = this.rewriteBlockGroup(raw, rawClusterTS, fixedClusterTS);
          clusterBody.pushElement(ID.BLOCK_GROUP, rewritten);
          break;
        }
        case ID.PREV_SIZE:
          // Drop PrevSize — it will be wrong after any rewrite.
          break;
        default:
          clusterBody.pushElement(idR.id, src.subarray(i, i + szR.value));
          break;
      }
      if (szR.value === UNKNOWN_SIZE) break;
      i += szR.value;
    }

    // Record the cluster's offset within the Segment body for Cues.
    // We store it as the offset of the Cluster element itself (id+size+body).
    const clusterBodyBytes = clusterBody.byteLength;
    const clusterElemSize  = encodeID(ID.CLUSTER).length + encodeVint(clusterBodyBytes).length + clusterBodyBytes;

    // Patch Cue cluster positions: the last pushed cue point (if it matches
    // this cluster's first keyframe) gets the current offset.
    if (this.cuePoints.length > 0) {
      const lastCue = this.cuePoints[this.cuePoints.length - 1];
      if (lastCue.clusterOffset === undefined || lastCue.clusterOffset < 0) {
        lastCue.clusterOffset = this.out.byteLength;
      }
    }
    // Update clusterOffset properly
    if (firstVideoKeyframe === false && this.cuePoints.length > 0) {
      this.cuePoints[this.cuePoints.length - 1].clusterOffset = this.out.byteLength;
    }

    this.out.pushElement(ID.CLUSTER, clusterBody.toUint8Array());
  }

  _seekHeadSize() {
    return 0; // offsets within segment body are computed relative to segBodyStart
  }

  // ── Block timestamp rewrite (audio only) ──────────────────────────────────

  rewriteBlock(data, rawClusterTS, fixedClusterTS) {
    const tnR    = readVint(data, 0);
    if (tnR.width + 2 >= data.length) return data;

    const trackNum  = tnR.value;
    const tsOffset  = tnR.width;
    const origRelTS = readInt16(data, tsOffset);
    const origAbsTS = rawClusterTS + origRelTS;

    const track = this.sr.tracks.get(trackNum);
    if (!track || !track.isAudio || track.sampleRate === 0) {
      // Video: just adjust relTS for monotonicity shift.
      const deltaTS = fixedClusterTS - rawClusterTS;
      if (deltaTS === 0) return data;
      const out = new Uint8Array(data);
      const newRelTS = origRelTS - deltaTS;
      const tsBytes = encodeInt16(newRelTS);
      out[tsOffset] = tsBytes[0]; out[tsOffset + 1] = tsBytes[1];
      return out;
    }

    // Audio: recompute from sample counter.
    const newAbsTS = this.computeNewAudioTS(track);
    const newRelTS = newAbsTS - fixedClusterTS;

    // Extract Opus payload (after tracknum vint + 2-byte TS + 1-byte flags)
    const payloadStart = tnR.width + 3;
    const payload = data.length > payloadStart ? data.subarray(payloadStart) : null;
    this.advanceSamples(track, payload);

    const out = new Uint8Array(data);
    const tsBytes = encodeInt16(newRelTS);
    out[tsOffset] = tsBytes[0]; out[tsOffset + 1] = tsBytes[1];
    return out;
  }

  rewriteBlockGroup(data, rawClusterTS, fixedClusterTS) {
    const out = new Chunks();
    let i = 0;
    while (i < data.length) {
      let idR, szR;
      try { idR = readID(data, i); i += idR.width; } catch { break; }
      try { szR = readVint(data, i); i += szR.width; } catch { break; }
      const body = data.subarray(i, i + szR.value);
      if (idR.id === ID.BLOCK) {
        out.pushElement(idR.id, this.rewriteBlock(body, rawClusterTS, fixedClusterTS));
      } else {
        out.pushElement(idR.id, body);
      }
      i += szR.value;
    }
    return out.toUint8Array();
  }

  // ── Audio PTS arithmetic ──────────────────────────────────────────────────

  computeNewAudioTS(track) {
    const samples = this.audioSamples.get(track.number) || 0;
    return Math.round(samples / track.sampleRate * 1e9 / this.sr.timestampScale);
  }

  advanceSamples(track, opusPayload) {
    const prev    = this.audioSamples.get(track.number) || 0;
    const samples = opusSamplesInBlock(opusPayload, track.sampleRate);
    this.audioSamples.set(track.number, prev + samples);
  }

  // ── Cues ──────────────────────────────────────────────────────────────────

  buildCues() {
    const cues = new Chunks();
    for (const cp of this.cuePoints) {
      // CueTrackPositions
      const ctp = new Chunks();
      ctp.pushElement(ID.CUE_TRACK,       encodeUintFixed(cp.cueTrack, 1));
      ctp.pushElement(ID.CUE_CLUSTER_POS, encodeUintFixed(cp.clusterOffset, 6));

      const cuePoint = new Chunks();
      cuePoint.pushElement(ID.CUE_TIME,      encodeUintFixed(cp.cueTime, 4));
      cuePoint.pushElement(ID.CUE_TRACK_POS, ctp.toUint8Array());

      cues.pushElement(ID.CUE_POINT, cuePoint.toUint8Array());
    }
    return cues.toUint8Array();
  }

  // ── SeekHead ──────────────────────────────────────────────────────────────

  buildSeekHead(infoOffset, tracksOffset, cuesOffset) {
    const makeSeek = (rawID, offset) => {
      // SeekID is the element ID encoded as binary.
      const seekIDBytes = encodeID(rawID);
      const seek        = new Chunks();
      seek.pushElement(ID.SEEK_ID,       seekIDBytes);
      seek.pushElement(ID.SEEK_POSITION, encodeUintFixed(offset, 6));
      const seekBody = seek.toUint8Array();
      const seekEl   = new Uint8Array(encodeID(ID.SEEK).length + encodeVint(seekBody.length).length + seekBody.length);
      let off = 0;
      seekEl.set(encodeID(ID.SEEK), off); off += encodeID(ID.SEEK).length;
      seekEl.set(encodeVint(seekBody.length), off); off += encodeVint(seekBody.length).length;
      seekEl.set(seekBody, off);
      return seekEl;
    };

    const seekInfo   = makeSeek(ID.INFO,   infoOffset);
    const seekTracks = makeSeek(ID.TRACKS, tracksOffset);
    const seekCues   = makeSeek(ID.CUES,   cuesOffset);

    let seekBodySize = seekInfo.length + seekTracks.length + seekCues.length;

    // Pad to SEEKHEAD_RESERVED_BODY with a Void element.
    const padding = SEEKHEAD_RESERVED_BODY - seekBodySize;
    const seekHeadBody = new Uint8Array(SEEKHEAD_RESERVED_BODY);
    let off = 0;
    seekHeadBody.set(seekInfo,   off); off += seekInfo.length;
    seekHeadBody.set(seekTracks, off); off += seekTracks.length;
    seekHeadBody.set(seekCues,   off); off += seekCues.length;
    if (padding > 0) {
      // Void element to fill remaining space.
      const voidID   = encodeID(ID.VOID);
      const voidBody = padding - voidID.length - encodeVint(padding - voidID.length - 1).length;
      if (voidBody >= 0) {
        seekHeadBody.set(voidID, off); off += voidID.length;
        seekHeadBody.set(encodeVint(voidBody), off); off += encodeVint(voidBody).length;
        // rest is already zero-filled (valid Void body)
      }
    }

    // Wrap in SeekHead element.
    const shID   = encodeID(ID.SEEK_HEAD);
    const shSize = encodeVint(SEEKHEAD_RESERVED_BODY);
    const result = new Uint8Array(shID.length + shSize.length + SEEKHEAD_RESERVED_BODY);
    let roff = 0;
    result.set(shID,       roff); roff += shID.length;
    result.set(shSize,     roff); roff += shSize.length;
    result.set(seekHeadBody, roff);
    return result;
  }
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

/** Encode value as an 8-byte EBML data-size VINT (always 8 bytes wide). */
function encode8ByteVint(v) {
  const out = new Uint8Array(8);
  out[0] = 0x01; // 8-byte marker
  for (let i = 7; i >= 1; i--) { out[i] = v & 0xFF; v = Math.floor(v / 256); }
  return out;
}

/**
 * Parse an Opus packet (WebM block payload, after the 4-byte WebM block header)
 * and return the total number of PCM samples it contains.
 *
 * Opus TOC byte layout:
 *   bits 7-3 : config (0-31) → frame duration lookup
 *   bit  2   : stereo flag
 *   bits 1-0 : code
 *     0 = 1 frame
 *     1 = 2 frames, equal size
 *     2 = 2 frames, different size
 *     3 = CBR/VBR multi-frame, next byte = frame count (M)
 *
 * Frame duration by config:
 *   0-3   → 10ms   (NB SILK)
 *   4-7   → 20ms
 *   8-11  → 40ms
 *   12-15 → 60ms
 *   16-19 → 2.5ms  (CELT)
 *   20-23 → 5ms
 *   24-27 → 10ms
 *   28-31 → 20ms
 */
function opusSamplesInBlock(payload, sampleRate) {
  if (!payload || payload.length < 1) return Math.round(sampleRate * 0.020);
  const toc    = payload[0];
  const config = (toc >> 3) & 0x1F;
  const code   = toc & 0x03;

  // Frame duration in ms per config index
  const frameDurMs = [
    10,20,40,60, 10,20,40,60, 10,20,40,60, 10,20,40,60, // 0-15
    2.5,5,10,20, 2.5,5,10,20, 2.5,5,10,20, 2.5,5,10,20  // 16-31
  ][config];

  let frameCount;
  if (code === 0) {
    frameCount = 1;
  } else if (code === 1 || code === 2) {
    frameCount = 2;
  } else {
    // code === 3: multi-frame, frame count in next byte bits 0-5
    frameCount = payload.length > 1 ? (payload[1] & 0x3F) : 1;
    if (frameCount === 0) frameCount = 1;
  }

  return Math.round(sampleRate * frameDurMs / 1000) * frameCount;
}

/** Duration of one Opus frame in ticks. */
function defaultFrameTicks(sr) {
  // 20 ms in ticks = 20_000_000 ns / timestampScale
  return Math.round(20_000_000 / sr.timestampScale);
}

function isVideoKeyframe(clusterBuf, blockBodyStart, blockBodySize, sr) {
  if (blockBodySize < 4) return false;
  const tnR = readVint(clusterBuf, blockBodyStart);
  const track = sr.tracks.get(tnR.value);
  if (!track || !track.isVideo) return false;
  // SimpleBlock flags byte: bit 7 = keyframe
  const flagsOffset = blockBodyStart + tnR.width + 2;
  return (clusterBuf[flagsOffset] & 0x80) !== 0;
}

function getTrackNum(buf, pos) {
  return readVint(buf, pos).value;
}

// ─────────────────────────────────────────────────────────────────────────────
// Public API
// ─────────────────────────────────────────────────────────────────────────────

/**
 * Fix a Chrome MediaRecorder WebM Blob.
 *
 * Performs a two-pass process:
 *   Pass 1 (scan)  — collect track metadata, last timestamp, cluster list
 *   Pass 2 (write) — emit fixed output
 *
 * @param   {Blob}          blob  Raw blob from MediaRecorder or chunked assembly
 * @returns {Promise<Blob>}       Fixed WebM blob, ready to upload or play
 */
async function fixWebM(blob) {
  const buf = new Uint8Array(await blob.arrayBuffer());

  // Pass 1: scan
  const sr = scan(buf);

  // Pass 2: rewrite
  const rewriter = new Rewriter(buf, sr);
  return rewriter.rewrite();
}

// ─────────────────────────────────────────────────────────────────────────────
// Drop-in MediaRecorder wrapper
// ─────────────────────────────────────────────────────────────────────────────

/**
 * WebMRecorder wraps MediaRecorder and automatically applies all fixes
 * when recording stops.
 *
 * @example
 *   const rec = new WebMRecorder(stream, { mimeType: 'video/webm;codecs=vp8,opus' });
 *   rec.start();
 *   // ... later ...
 *   rec.stop();
 *   const fixed = await rec.getFixedBlob();
 *   await upload(fixed);
 */
class WebMRecorder {
  #recorder;
  #chunks = [];
  #fixedBlobPromise;
  #resolve;

  constructor(stream, options = {}) {
    this.#recorder = new MediaRecorder(stream, options);
    this.#recorder.ondataavailable = (e) => {
      if (e.data?.size > 0) this.#chunks.push(e.data);
    };
    this.#recorder.onstop = async () => {
      const raw = new Blob(this.#chunks, { type: this.#recorder.mimeType });
      try { this.#resolve(await fixWebM(raw)); }
      catch (err) {
        console.warn('webm-fix: falling back to unfixed blob', err);
        this.#resolve(raw);
      }
    };
    this.#fixedBlobPromise = new Promise(r => { this.#resolve = r; });
  }

  start(timeslice)  { this.#chunks = []; this.#recorder.start(timeslice); }
  stop()            { this.#recorder.stop(); }
  pause()           { this.#recorder.pause(); }
  resume()          { this.#recorder.resume(); }
  get state()       { return this.#recorder.state; }
  get mimeType()    { return this.#recorder.mimeType; }
  getFixedBlob()    { return this.#fixedBlobPromise; }
}