/**
 * Dependency-free QR Code encoder — byte mode, EC level L, versions 1-10
 * (274 bytes max). ISO/IEC 18004.
 *
 * It exists for one reason: TOTP enrollment tells the user to "scan this in
 * your authenticator app" and there was nothing to scan. Adding a native QR
 * dependency for a 30-line-of-payload use case is not worth it, so the encoder
 * lives here and renders with plain <View>s (see QrCode.tsx).
 */

interface VersionSpec {
  /** EC codewords per block. */
  ec: number
  /** Data-codeword groups: [blockCount, dataCodewordsPerBlock][] */
  groups: Array<[number, number]>
}

// ECC level L, versions 1-10. Table 13-22 of the spec.
const VERSIONS: VersionSpec[] = [
  { ec: 7, groups: [[1, 19]] }, // v1
  { ec: 10, groups: [[1, 34]] },
  { ec: 15, groups: [[1, 55]] },
  { ec: 20, groups: [[1, 80]] },
  { ec: 26, groups: [[1, 108]] },
  { ec: 18, groups: [[2, 68]] },
  { ec: 20, groups: [[2, 78]] },
  { ec: 24, groups: [[2, 97]] },
  { ec: 30, groups: [[2, 116]] },
  { ec: 18, groups: [[2, 68], [2, 69]] }, // v10
]

/** Alignment-pattern centre coordinates, indexed by version - 1. */
const ALIGNMENT: number[][] = [
  [],
  [6, 18],
  [6, 22],
  [6, 26],
  [6, 30],
  [6, 34],
  [6, 22, 38],
  [6, 24, 42],
  [6, 26, 46],
  [6, 28, 50],
]

// --- GF(256), primitive polynomial 0x11d -----------------------------------

const EXP = new Uint8Array(512)
const LOG = new Uint8Array(256)
;(() => {
  let x = 1
  for (let i = 0; i < 255; i++) {
    EXP[i] = x
    LOG[x] = i
    x <<= 1
    if (x & 0x100) x ^= 0x11d
  }
  for (let i = 255; i < 512; i++) EXP[i] = EXP[i - 255]
})()

function gfMul(a: number, b: number): number {
  if (a === 0 || b === 0) return 0
  return EXP[LOG[a] + LOG[b]]
}

/** Generator polynomial of degree `degree`, highest-order coefficient first. */
function rsGenerator(degree: number): number[] {
  let poly = [1]
  for (let i = 0; i < degree; i++) {
    const next = new Array<number>(poly.length + 1).fill(0)
    for (let j = 0; j < poly.length; j++) {
      next[j] ^= poly[j]
      next[j + 1] ^= gfMul(poly[j], EXP[i])
    }
    poly = next
  }
  return poly
}

/** Reed-Solomon remainder — the EC codewords for one block. */
function rsEncode(data: number[], ecLen: number): number[] {
  const gen = rsGenerator(ecLen)
  const rem = new Array<number>(ecLen).fill(0)
  for (const byte of data) {
    const factor = byte ^ rem[0]
    rem.shift()
    rem.push(0)
    if (factor !== 0) {
      for (let i = 0; i < ecLen; i++) rem[i] ^= gfMul(gen[i + 1], factor)
    }
  }
  return rem
}

// --- BCH check bits ---------------------------------------------------------

/**
 * GF(2) polynomial remainder of `value` (already shifted left by `genDegree`)
 * divided by `generator`.
 */
function bchRemainder(value: number, generator: number, genDegree: number, dataBits: number): number {
  let rem = value
  for (let i = dataBits - 1; i >= 0; i--) {
    if ((rem >> (genDegree + i)) & 1) rem ^= generator << i
  }
  return rem
}

/** 15-bit format information for EC level L and the given mask. */
function formatBits(mask: number): number {
  const data = (0b01 << 3) | mask // 01 = level L
  const value = data << 10
  return (value | bchRemainder(value, 0b10100110111, 10, 5)) ^ 0b101010000010010
}

/** 18-bit version information (only present from version 7). */
function versionBits(version: number): number {
  const value = version << 12
  return value | bchRemainder(value, 0b1111100100101, 12, 6)
}

// --- Bit buffer -------------------------------------------------------------

class Bits {
  readonly bits: number[] = []
  push(value: number, length: number) {
    for (let i = length - 1; i >= 0; i--) this.bits.push((value >>> i) & 1)
  }
}

function utf8Bytes(text: string): number[] {
  const out: number[] = []
  for (const ch of text) {
    let cp = ch.codePointAt(0) as number
    if (cp < 0x80) out.push(cp)
    else if (cp < 0x800) out.push(0xc0 | (cp >> 6), 0x80 | (cp & 0x3f))
    else if (cp < 0x10000) out.push(0xe0 | (cp >> 12), 0x80 | ((cp >> 6) & 0x3f), 0x80 | (cp & 0x3f))
    else {
      out.push(
        0xf0 | (cp >> 18),
        0x80 | ((cp >> 12) & 0x3f),
        0x80 | ((cp >> 6) & 0x3f),
        0x80 | (cp & 0x3f),
      )
    }
  }
  return out
}

function totalDataCodewords(spec: VersionSpec): number {
  return spec.groups.reduce((sum, [blocks, size]) => sum + blocks * size, 0)
}

// --- Matrix -----------------------------------------------------------------

type Grid = { size: number; modules: Uint8Array; reserved: Uint8Array }

function grid(size: number): Grid {
  return { size, modules: new Uint8Array(size * size), reserved: new Uint8Array(size * size) }
}

function setModule(g: Grid, x: number, y: number, dark: boolean, reserve = true) {
  if (x < 0 || y < 0 || x >= g.size || y >= g.size) return
  g.modules[y * g.size + x] = dark ? 1 : 0
  if (reserve) g.reserved[y * g.size + x] = 1
}

function isDark(g: Grid, x: number, y: number): boolean {
  return g.modules[y * g.size + x] === 1
}

function placeFinder(g: Grid, cx: number, cy: number) {
  for (let dy = -1; dy <= 7; dy++) {
    for (let dx = -1; dx <= 7; dx++) {
      const x = cx + dx
      const y = cy + dy
      if (x < 0 || y < 0 || x >= g.size || y >= g.size) continue
      const inRing = dx >= 0 && dx <= 6 && dy >= 0 && dy <= 6
      const border = inRing && (dx === 0 || dx === 6 || dy === 0 || dy === 6)
      const core = inRing && dx >= 2 && dx <= 4 && dy >= 2 && dy <= 4
      setModule(g, x, y, border || core)
    }
  }
}

function placeFunctionPatterns(g: Grid, version: number) {
  const last = g.size - 7

  placeFinder(g, 0, 0)
  placeFinder(g, last, 0)
  placeFinder(g, 0, last)

  // Timing patterns.
  for (let i = 8; i < g.size - 8; i++) {
    setModule(g, i, 6, i % 2 === 0)
    setModule(g, 6, i, i % 2 === 0)
  }

  // Alignment patterns, skipping the three finder corners.
  const centres = ALIGNMENT[version - 1]
  for (const cy of centres) {
    for (const cx of centres) {
      const nearFinder =
        (cx <= 8 && cy <= 8) || (cx <= 8 && cy >= g.size - 9) || (cx >= g.size - 9 && cy <= 8)
      if (nearFinder) continue
      for (let dy = -2; dy <= 2; dy++) {
        for (let dx = -2; dx <= 2; dx++) {
          const ring = Math.max(Math.abs(dx), Math.abs(dy))
          setModule(g, cx + dx, cy + dy, ring !== 1)
        }
      }
    }
  }

  // Dark module.
  setModule(g, 8, g.size - 8, true)

  // Reserve the format-information areas (written after masking). Index 6 is
  // skipped in both strips: (6,8) and (8,6) belong to the timing patterns, and
  // blanking them here left two light modules where the timing must be dark.
  for (let i = 0; i < 9; i++) {
    if (i !== 6) {
      setModule(g, i, 8, false)
      setModule(g, 8, i, false)
    }
  }
  for (let i = 0; i < 8; i++) {
    setModule(g, g.size - 1 - i, 8, false)
    setModule(g, 8, g.size - 1 - i, false)
  }

  // Reserve the version-information areas.
  if (version >= 7) {
    for (let i = 0; i < 18; i++) {
      const a = Math.floor(i / 3)
      const b = (i % 3) + g.size - 11
      setModule(g, a, b, false)
      setModule(g, b, a, false)
    }
  }
}

function placeData(g: Grid, bits: number[]) {
  let i = 0
  let upward = true
  for (let right = g.size - 1; right >= 1; right -= 2) {
    if (right === 6) right = 5 // the vertical timing column is never data
    for (let step = 0; step < g.size; step++) {
      const y = upward ? g.size - 1 - step : step
      for (const x of [right, right - 1]) {
        if (g.reserved[y * g.size + x]) continue
        g.modules[y * g.size + x] = i < bits.length ? bits[i] : 0
        i++
      }
    }
    upward = !upward
  }
}

const MASKS: Array<(x: number, y: number) => boolean> = [
  (x, y) => (x + y) % 2 === 0,
  (_x, y) => y % 2 === 0,
  (x) => x % 3 === 0,
  (x, y) => (x + y) % 3 === 0,
  (x, y) => (Math.floor(y / 2) + Math.floor(x / 3)) % 2 === 0,
  (x, y) => ((x * y) % 2) + ((x * y) % 3) === 0,
  (x, y) => (((x * y) % 2) + ((x * y) % 3)) % 2 === 0,
  (x, y) => (((x + y) % 2) + ((x * y) % 3)) % 2 === 0,
]

function applyMask(g: Grid, mask: number) {
  const fn = MASKS[mask]
  for (let y = 0; y < g.size; y++) {
    for (let x = 0; x < g.size; x++) {
      if (g.reserved[y * g.size + x]) continue
      if (fn(x, y)) g.modules[y * g.size + x] ^= 1
    }
  }
}

function writeFormat(g: Grid, mask: number) {
  const bitsValue = formatBits(mask)
  for (let i = 0; i < 15; i++) {
    const bit = ((bitsValue >> i) & 1) === 1
    // Copy 1 — around the top-left finder.
    if (i < 6) setModule(g, 8, i, bit)
    else if (i === 6) setModule(g, 8, 7, bit)
    else if (i === 7) setModule(g, 8, 8, bit)
    else if (i === 8) setModule(g, 7, 8, bit)
    else setModule(g, 14 - i, 8, bit)
    // Copy 2 — split between the other two finders.
    if (i < 8) setModule(g, g.size - 1 - i, 8, bit)
    else setModule(g, 8, g.size - 15 + i, bit)
  }
  setModule(g, 8, g.size - 8, true) // dark module
}

function writeVersion(g: Grid, version: number) {
  if (version < 7) return
  const value = versionBits(version)
  for (let i = 0; i < 18; i++) {
    const bit = ((value >> i) & 1) === 1
    const a = Math.floor(i / 3)
    const b = (i % 3) + g.size - 11
    setModule(g, a, b, bit)
    setModule(g, b, a, bit)
  }
}

function penalty(g: Grid): number {
  const n = g.size
  let score = 0

  // Rule 1 — runs of five or more same-coloured modules.
  for (let i = 0; i < n; i++) {
    for (const horizontal of [true, false]) {
      let run = 1
      for (let j = 1; j < n; j++) {
        const prev = horizontal ? isDark(g, j - 1, i) : isDark(g, i, j - 1)
        const cur = horizontal ? isDark(g, j, i) : isDark(g, i, j)
        if (cur === prev) {
          run++
          if (run === 5) score += 3
          else if (run > 5) score += 1
        } else run = 1
      }
    }
  }

  // Rule 2 — 2x2 blocks of one colour.
  for (let y = 0; y < n - 1; y++) {
    for (let x = 0; x < n - 1; x++) {
      const c = isDark(g, x, y)
      if (c === isDark(g, x + 1, y) && c === isDark(g, x, y + 1) && c === isDark(g, x + 1, y + 1)) {
        score += 3
      }
    }
  }

  // Rule 3 — finder-like 1:1:3:1:1 patterns.
  const pattern = [true, false, true, true, true, false, true]
  const matches = (get: (k: number) => boolean, start: number): boolean => {
    for (let k = 0; k < 7; k++) if (get(start + k) !== pattern[k]) return false
    return true
  }
  const clearRun = (get: (k: number) => boolean, from: number, to: number): boolean => {
    for (let k = from; k < to; k++) {
      if (k < 0 || k >= n) continue
      if (get(k)) return false
    }
    return true
  }
  for (let i = 0; i < n; i++) {
    for (const horizontal of [true, false]) {
      const get = (k: number) => (horizontal ? isDark(g, k, i) : isDark(g, i, k))
      for (let j = 0; j <= n - 7; j++) {
        if (!matches(get, j)) continue
        if (clearRun(get, j - 4, j) || clearRun(get, j + 7, j + 11)) score += 40
      }
    }
  }

  // Rule 4 — deviation from a 50% dark ratio.
  let dark = 0
  for (let i = 0; i < g.modules.length; i++) if (g.modules[i]) dark++
  const percent = (dark * 100) / (n * n)
  score += Math.floor(Math.abs(percent - 50) / 5) * 10

  return score
}

// --- Public API -------------------------------------------------------------

/**
 * Encodes `text` and returns the module matrix as rows of booleans
 * (`true` = dark). Throws when the payload exceeds version 10 at level L.
 */
export function encodeQr(text: string): boolean[][] {
  const bytes = utf8Bytes(text)

  let version = 0
  let spec: VersionSpec | null = null
  for (let v = 1; v <= VERSIONS.length; v++) {
    const candidate = VERSIONS[v - 1]
    const countBits = v < 10 ? 8 : 16
    if (4 + countBits + bytes.length * 8 <= totalDataCodewords(candidate) * 8) {
      version = v
      spec = candidate
      break
    }
  }
  if (!spec) throw new Error('payload too large for a version-10 QR code')

  // 1. Bit stream: mode indicator, character count, payload, terminator, padding.
  const buf = new Bits()
  buf.push(0b0100, 4)
  buf.push(bytes.length, version < 10 ? 8 : 16)
  for (const b of bytes) buf.push(b, 8)

  const capacityBits = totalDataCodewords(spec) * 8
  buf.push(0, Math.min(4, capacityBits - buf.bits.length))
  while (buf.bits.length % 8 !== 0) buf.bits.push(0)

  const dataCodewords: number[] = []
  for (let i = 0; i < buf.bits.length; i += 8) {
    let byte = 0
    for (let j = 0; j < 8; j++) byte = (byte << 1) | buf.bits[i + j]
    dataCodewords.push(byte)
  }
  for (let pad = 0; dataCodewords.length < totalDataCodewords(spec); pad++) {
    dataCodewords.push(pad % 2 === 0 ? 0xec : 0x11)
  }

  // 2. Split into blocks and compute error correction per block.
  const dataBlocks: number[][] = []
  const ecBlocks: number[][] = []
  let offset = 0
  for (const [blocks, size] of spec.groups) {
    for (let b = 0; b < blocks; b++) {
      const block = dataCodewords.slice(offset, offset + size)
      offset += size
      dataBlocks.push(block)
      ecBlocks.push(rsEncode(block, spec.ec))
    }
  }

  // 3. Interleave.
  const interleaved: number[] = []
  const maxData = Math.max(...dataBlocks.map((b) => b.length))
  for (let i = 0; i < maxData; i++) {
    for (const block of dataBlocks) if (i < block.length) interleaved.push(block[i])
  }
  for (let i = 0; i < spec.ec; i++) {
    for (const block of ecBlocks) interleaved.push(block[i])
  }

  const bitStream: number[] = []
  for (const byte of interleaved) {
    for (let i = 7; i >= 0; i--) bitStream.push((byte >> i) & 1)
  }

  // 4. Build the matrix and pick the lowest-penalty mask.
  const size = 17 + version * 4
  let best: Grid | null = null
  let bestScore = Infinity
  for (let mask = 0; mask < 8; mask++) {
    const g = grid(size)
    placeFunctionPatterns(g, version)
    placeData(g, bitStream)
    applyMask(g, mask)
    writeFormat(g, mask)
    writeVersion(g, version)
    const score = penalty(g)
    if (score < bestScore) {
      bestScore = score
      best = g
    }
  }

  const out: boolean[][] = []
  const g = best as Grid
  for (let y = 0; y < size; y++) {
    const row: boolean[] = []
    for (let x = 0; x < size; x++) row.push(isDark(g, x, y))
    out.push(row)
  }
  return out
}
