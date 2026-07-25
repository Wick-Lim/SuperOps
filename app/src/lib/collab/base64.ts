/**
 * Base64 for CRDT payloads.
 *
 * The wire form is base64 because the frame is JSON and Go's encoding/json
 * renders a []byte that way — that is the only reason an opaque binary update
 * can ride a JSON frame at all.
 *
 * Written by hand rather than reached for from a library because the two
 * runtimes this bundle targets disagree: React Native has no `Buffer` and, on
 * older engines, no `atob`/`btoa`; the web has both but `btoa` operates on a
 * binary STRING, so the obvious `btoa(String.fromCharCode(...bytes))` blows the
 * argument limit on a large paste — which is precisely the case the HTTP
 * fallback exists for. So: chunked, and no spread.
 */

const ALPHABET = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/'

/** Lookup table, built once. Index 255 means "not a base64 character". */
const DECODE = (() => {
  const t = new Uint8Array(256).fill(255)
  for (let i = 0; i < ALPHABET.length; i++) t[ALPHABET.charCodeAt(i)] = i
  return t
})()

export function toBase64(bytes: Uint8Array): string {
  let out = ''
  const len = bytes.length
  let i = 0
  for (; i + 2 < len; i += 3) {
    const n = (bytes[i] << 16) | (bytes[i + 1] << 8) | bytes[i + 2]
    out +=
      ALPHABET[(n >> 18) & 63] + ALPHABET[(n >> 12) & 63] + ALPHABET[(n >> 6) & 63] + ALPHABET[n & 63]
  }
  const rest = len - i
  if (rest === 1) {
    const n = bytes[i] << 16
    out += ALPHABET[(n >> 18) & 63] + ALPHABET[(n >> 12) & 63] + '=='
  } else if (rest === 2) {
    const n = (bytes[i] << 16) | (bytes[i + 1] << 8)
    out += ALPHABET[(n >> 18) & 63] + ALPHABET[(n >> 12) & 63] + ALPHABET[(n >> 6) & 63] + '='
  }
  return out
}

export function fromBase64(text: string): Uint8Array {
  // Length is computed from the padding rather than assumed, because a decoder
  // that over-allocated would hand Yjs trailing zero bytes and Yjs would read
  // them as document content.
  let end = text.length
  while (end > 0 && text[end - 1] === '=') end--

  const out = new Uint8Array(Math.floor((end * 3) / 4))
  let o = 0
  let acc = 0
  let bits = 0
  for (let i = 0; i < end; i++) {
    const v = DECODE[text.charCodeAt(i)]
    if (v === 255) continue // whitespace or a stray newline; not an error
    acc = (acc << 6) | v
    bits += 6
    if (bits >= 8) {
      bits -= 8
      out[o++] = (acc >> bits) & 0xff
    }
  }
  return o === out.length ? out : out.subarray(0, o)
}
