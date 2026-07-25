import * as Crypto from 'expo-crypto'

/**
 * The Web Crypto surface Yjs needs on React Native.
 *
 * lib0 — Yjs's utility layer — imports `isomorphic-webcrypto/src/react-native`
 * in its native build, and that package is neither installed nor worth
 * installing: it is unmaintained and pulls in a pure-JS crypto implementation
 * for an API Expo already provides natively. Without a substitute the iOS and
 * Android bundles fail to resolve at all, which is how this was found — the
 * export died on `import "lib0/webcrypto"`.
 *
 * metro.config.js redirects that import here for every non-web platform. Web
 * keeps lib0's own browser build, which reads the real `globalThis.crypto`.
 *
 * Only `getRandomValues` is actually reachable: Yjs uses it to mint a client
 * ID, and touches `subtle` nowhere. It is exposed anyway so that the shape
 * matches what lib0's module expects — a missing property would be a runtime
 * `undefined` at import time rather than an error anybody could read.
 */
export const getRandomValues = <T extends Uint8Array | Int8Array | Uint16Array | Int16Array | Uint32Array | Int32Array>(
  array: T,
): T => Crypto.getRandomValues(array)

/**
 * Deliberately absent rather than faked.
 *
 * expo-crypto has no SubtleCrypto, and a stub that returned plausible-looking
 * bytes would be worse than nothing: a caller would believe it had encrypted
 * something. Nothing in the collaboration path calls it, and if something ever
 * does, this throws where the problem is instead of silently producing garbage.
 */
export const subtle = new Proxy(
  {},
  {
    get(_target, prop) {
      throw new Error(
        `SubtleCrypto.${String(prop)} is not available on this platform; ` +
          'nothing in the collaboration path should be calling it',
      )
    },
  },
) as SubtleCrypto

export default { getRandomValues, subtle }
