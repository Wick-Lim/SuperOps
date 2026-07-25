const { getDefaultConfig } = require('expo/metro-config')
const path = require('path')

const config = getDefaultConfig(__dirname)

/**
 * Redirect lib0's React Native crypto import.
 *
 * Yjs's utility layer (lib0) resolves `lib0/webcrypto` to a build that imports
 * `isomorphic-webcrypto/src/react-native`. That package is not a dependency of
 * this app and is not worth adding — it is unmaintained and ships a pure-JS
 * implementation of an API Expo provides natively — so the iOS and Android
 * bundles simply failed to resolve.
 *
 * This is a resolver rule rather than an `npm install` because the goal is to
 * REMOVE a dependency, not add one. Web is untouched: lib0's browser build
 * reads the real `globalThis.crypto` there.
 */
const LIB0_RN_CRYPTO = 'isomorphic-webcrypto/src/react-native'
const SHIM = path.resolve(__dirname, 'src/lib/collab/webcrypto.ts')

const upstreamResolve = config.resolver.resolveRequest

config.resolver.resolveRequest = (context, moduleName, platform) => {
  if (platform !== 'web' && moduleName === LIB0_RN_CRYPTO) {
    return { type: 'sourceFile', filePath: SHIM }
  }
  return (upstreamResolve ?? context.resolveRequest)(context, moduleName, platform)
}

module.exports = config
