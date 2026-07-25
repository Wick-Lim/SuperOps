import React, { useCallback, useMemo, useState } from 'react'
import { View, Text, TextInput, Pressable, ScrollView, StyleSheet } from 'react-native'
import { theme } from '../lib/theme'
import { space, MIN_TOUCH } from '../lib/responsive'
import { DesignModel, type DesignNode, type NodeKind } from '../lib/design/model'

/**
 * The design surface.
 *
 * Scoped deliberately: frames, rectangles, ellipses, lines and text, with
 * selection, geometry and colour editing. That is the shape vocabulary a
 * product team actually uses for a wireframe, and it is the subset whose CRDT
 * story is honest — every operation is a field write on one node, which two
 * people doing at once resolves correctly without a merge rule.
 *
 * NOT here, and each is a cut rather than a gap: vector paths and boolean ops
 * (a path is an ordered point list, and concurrent edits to one need the same
 * treatment as text), components and instances (an override model is its own
 * design), auto-layout (a constraint solver whose result must be identical on
 * every client), and export to PNG/SVG (a renderer).
 */

export interface CanvasProps {
  model: DesignModel
  editable: boolean
  /** Bumped by the caller on every document change. */
  revision: number
  onEdit?: () => void
}

const TOOLS: { kind: NodeKind; label: string }[] = [
  { kind: 'frame', label: 'Frame' },
  { kind: 'rect', label: 'Rectangle' },
  { kind: 'ellipse', label: 'Ellipse' },
  { kind: 'text', label: 'Text' },
  { kind: 'line', label: 'Line' },
]

const PALETTE = ['#3b82c4', '#3fa66a', '#c9873a', '#e5534b', '#8b5cd6', '#5c6570', '#ffffff']

export default function Canvas({ model, editable, revision, onEdit }: CanvasProps) {
  const [selectedId, setSelectedId] = useState<string | null>(null)
  const nodes = useMemo(() => model.list(), [model, revision])
  const selected = selectedId ? nodes.find((n) => n.id === selectedId) ?? null : null

  const create = useCallback(
    (kind: NodeKind) => {
      if (!editable) return
      // A deterministic-enough id from the doc's own client id plus a counter,
      // rather than a random uuid: the client id is assigned by Yjs and is
      // already unique per session, so this cannot collide with a concurrent
      // creation on another client.
      const id = `${model.doc.clientID.toString(36)}-${model.nodes.size}-${kind}`
      const node = model.add(id, kind, {
        x: 80 + (model.nodes.size % 5) * 40,
        y: 80 + (model.nodes.size % 7) * 30,
        ...(kind === 'text' ? { w: 200, h: 32, fill: 'transparent', text: 'Text' } : {}),
        ...(kind === 'frame' ? { w: 320, h: 240, fill: '#ffffff', stroke: theme.border, strokeWidth: 1, text: 'Frame' } : {}),
        ...(kind === 'line' ? { h: 2, fill: theme.body } : {}),
      })
      if (node) {
        setSelectedId(node.id)
        onEdit?.()
      }
    },
    [editable, model, onEdit],
  )

  const patch = useCallback(
    (id: string, changes: Partial<DesignNode>) => {
      if (!editable) return
      model.update(id, changes)
      onEdit?.()
    },
    [editable, model, onEdit],
  )

  return (
    <View style={styles.root}>
      {editable && (
        <View style={styles.toolbar}>
          {TOOLS.map((t) => (
            <Pressable
              key={t.kind}
              onPress={() => create(t.kind)}
              style={({ pressed }) => [styles.tool, pressed && styles.pressed]}
              accessibilityRole="button"
            >
              <Text style={styles.toolText}>{t.label}</Text>
            </Pressable>
          ))}
        </View>
      )}

      <ScrollView style={styles.canvasScroll} contentContainerStyle={styles.canvas}>
        {nodes.length === 0 ? (
          <Text style={styles.empty}>
            {editable ? 'Add a frame or a shape to begin.' : 'This design is empty.'}
          </Text>
        ) : (
          nodes.map((node) => (
            <Pressable
              key={node.id}
              onPress={() => setSelectedId(node.id)}
              style={[shapeStyle(node), selectedId === node.id && styles.selected]}
              accessibilityRole="button"
              accessibilityLabel={`${node.kind}${node.text ? `: ${node.text}` : ''}`}
            >
              {node.text !== '' && (
                <Text
                  numberOfLines={node.kind === 'text' ? undefined : 1}
                  style={[styles.nodeText, { fontSize: node.fontSize }]}
                >
                  {node.text}
                </Text>
              )}
            </Pressable>
          ))
        )}
      </ScrollView>

      {selected && (
        <ScrollView style={styles.inspector} contentContainerStyle={styles.inspectorBody}>
          <Text style={styles.inspectorTitle}>{selected.kind}</Text>

          <Field
            label="Text"
            value={selected.text}
            editable={editable}
            onChange={(v) => patch(selected.id, { text: v })}
          />
          <View style={styles.numbers}>
            {(['x', 'y', 'w', 'h'] as const).map((k) => (
              <Field
                key={k}
                label={k.toUpperCase()}
                value={String(Math.round(selected[k]))}
                editable={editable}
                width={64}
                onChange={(v) => {
                  const n = Number(v)
                  if (Number.isFinite(n)) patch(selected.id, { [k]: n } as Partial<DesignNode>)
                }}
              />
            ))}
          </View>

          <Text style={styles.fieldLabel}>Fill</Text>
          <View style={styles.swatches}>
            {PALETTE.map((c) => (
              <Pressable
                key={c}
                onPress={() => patch(selected.id, { fill: c })}
                disabled={!editable}
                style={[styles.swatch, { backgroundColor: c }, selected.fill === c && styles.swatchOn]}
                accessibilityRole="button"
                accessibilityLabel={`Fill ${c}`}
              />
            ))}
          </View>

          {editable && (
            <Pressable
              onPress={() => {
                model.remove(selected.id)
                setSelectedId(null)
                onEdit?.()
              }}
              style={({ pressed }) => [styles.delete, pressed && styles.pressed]}
              accessibilityRole="button"
            >
              <Text style={styles.deleteText}>Delete</Text>
            </Pressable>
          )}
        </ScrollView>
      )}
    </View>
  )
}

function Field({
  label,
  value,
  editable,
  onChange,
  width,
}: {
  label: string
  value: string
  editable: boolean
  onChange: (v: string) => void
  width?: number
}) {
  const [draft, setDraft] = useState(value)
  // The shared value wins whenever it changes underneath: somebody else moving
  // the shape must not be overwritten by a stale local draft.
  React.useEffect(() => setDraft(value), [value])
  return (
    <View style={width ? { width } : undefined}>
      <Text style={styles.fieldLabel}>{label}</Text>
      <TextInput
        style={[styles.field, !editable && styles.readOnly]}
        value={draft}
        editable={editable}
        onChangeText={setDraft}
        onBlur={() => onChange(draft)}
        onSubmitEditing={() => onChange(draft)}
        accessibilityLabel={label}
      />
    </View>
  )
}

function shapeStyle(node: DesignNode) {
  return {
    position: 'absolute' as const,
    left: node.x,
    top: node.y,
    width: node.w,
    height: node.h,
    backgroundColor: node.fill === 'transparent' ? undefined : node.fill,
    borderColor: node.stroke || undefined,
    borderWidth: node.strokeWidth,
    borderRadius: node.kind === 'ellipse' ? Math.min(node.w, node.h) / 2 : node.kind === 'frame' ? 4 : 2,
    opacity: node.opacity,
    justifyContent: 'center' as const,
    paddingHorizontal: 6,
  }
}

const styles = StyleSheet.create({
  root: { flex: 1 },
  toolbar: {
    flexDirection: 'row',
    gap: space.xs,
    padding: space.sm,
    borderBottomWidth: StyleSheet.hairlineWidth,
    borderBottomColor: theme.border,
    backgroundColor: theme.surface,
    flexWrap: 'wrap',
  },
  tool: {
    paddingHorizontal: space.sm,
    minHeight: MIN_TOUCH - 8,
    justifyContent: 'center',
    backgroundColor: theme.surfaceAlt,
    borderRadius: 6,
  },
  toolText: { color: theme.body, fontSize: 13, fontWeight: '600' },
  pressed: { opacity: 0.7 },

  canvasScroll: { flex: 1, backgroundColor: theme.surfaceAlt },
  canvas: { minHeight: 700, minWidth: 700 },
  empty: { color: theme.textFaint, fontSize: 14, padding: space.lg },
  nodeText: { color: theme.text },
  selected: { borderWidth: 2, borderColor: theme.accent },

  inspector: {
    maxHeight: 260,
    borderTopWidth: StyleSheet.hairlineWidth,
    borderTopColor: theme.border,
    backgroundColor: theme.surface,
  },
  inspectorBody: { padding: space.sm, gap: 6 },
  inspectorTitle: { color: theme.textMuted, fontSize: 12, textTransform: 'uppercase', fontWeight: '700' },
  fieldLabel: { color: theme.textMuted, fontSize: 11, marginTop: 4 },
  field: {
    color: theme.text,
    fontSize: 14,
    backgroundColor: theme.bg,
    borderRadius: 6,
    paddingHorizontal: space.sm,
    paddingVertical: 6,
  },
  readOnly: { opacity: 0.6 },
  numbers: { flexDirection: 'row', gap: space.xs, flexWrap: 'wrap' },
  swatches: { flexDirection: 'row', gap: 6, flexWrap: 'wrap' },
  swatch: { width: 28, height: 28, borderRadius: 14, borderWidth: 1, borderColor: theme.border },
  swatchOn: { borderWidth: 3, borderColor: theme.accent },
  delete: {
    marginTop: space.sm,
    minHeight: MIN_TOUCH - 8,
    justifyContent: 'center',
    alignItems: 'center',
    borderRadius: 6,
    backgroundColor: theme.surfaceAlt,
  },
  deleteText: { color: theme.danger, fontSize: 14, fontWeight: '600' },
})
