import React, { useMemo } from 'react'
import { View, Text } from 'react-native'
import { encodeQr } from './qr'
import { theme } from '../../lib/theme'

interface Props {
  value: string
  /** Target edge length in points, including the quiet zone. */
  size?: number
  accessibilityLabel?: string
}

const QUIET_ZONE = 4

/**
 * Renders a QR code with plain Views — one View per horizontal run of dark
 * modules, which is roughly ten per row rather than one per module.
 *
 * A QR needs a light background and a quiet zone regardless of the app theme,
 * so this component is deliberately not theme-aware inside the code area.
 */
export default function QrCode({ value, size = 220, accessibilityLabel }: Props) {
  const runs = useMemo(() => {
    let matrix: boolean[][]
    try {
      matrix = encodeQr(value)
    } catch {
      return null
    }
    const modules = matrix.length + QUIET_ZONE * 2
    const out: Array<{ x: number; y: number; len: number }> = []
    matrix.forEach((row, y) => {
      let x = 0
      while (x < row.length) {
        if (!row[x]) {
          x++
          continue
        }
        let len = 1
        while (x + len < row.length && row[x + len]) len++
        out.push({ x: x + QUIET_ZONE, y: y + QUIET_ZONE, len })
        x += len
      }
    })
    return { modules, out }
  }, [value])

  if (!runs) {
    return (
      <View
        style={{
          height: size,
          alignItems: 'center',
          justifyContent: 'center',
          borderWidth: 1,
          borderColor: theme.borderStrong,
          borderRadius: 10,
        }}
      >
        <Text style={{ color: theme.textMuted, fontSize: 13, textAlign: 'center' }}>
          This code is too long to display. Add the secret manually instead.
        </Text>
      </View>
    )
  }

  // Snap to whole points so adjacent modules cannot leave sub-pixel seams that
  // confuse a scanner.
  const scale = Math.max(2, Math.floor(size / runs.modules))
  const edge = scale * runs.modules

  return (
    <View
      accessible
      accessibilityRole="image"
      accessibilityLabel={accessibilityLabel ?? 'QR code'}
      style={{ width: edge, height: edge, backgroundColor: '#ffffff', alignSelf: 'center' }}
    >
      {runs.out.map((run) => (
        <View
          key={`${run.y}-${run.x}`}
          style={{
            position: 'absolute',
            left: run.x * scale,
            top: run.y * scale,
            width: run.len * scale,
            height: scale,
            backgroundColor: '#000000',
          }}
        />
      ))}
    </View>
  )
}
