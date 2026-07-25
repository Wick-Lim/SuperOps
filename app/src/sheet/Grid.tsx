import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { View, Text, TextInput, ScrollView, Pressable, StyleSheet } from 'react-native'
import { theme } from '../lib/theme'
import { space } from '../lib/responsive'
import { Evaluator, display, isError, sourceOf } from '../lib/sheet/engine'
import { SheetModel, addressName, columnName, type Address } from '../lib/sheet/model'

/**
 * The grid.
 *
 * It renders DERIVED values and edits RAW input, and those are two different
 * things on screen at the same time: the selected cell shows what was typed so
 * it can be corrected, every other cell shows what it evaluates to. A grid that
 * showed formulas everywhere would be a source listing; one that showed values
 * everywhere would be uneditable.
 *
 * There is no virtualisation. The viewport is bounded to what a person can
 * actually look at (VIEW_ROWS × VIEW_COLS) and scrolled by changing the origin,
 * which is a smaller and more honest mechanism than a windowing library — and
 * it means the cell count on screen never depends on the sheet's size.
 */

const VIEW_ROWS = 40
const VIEW_COLS = 12
const CELL_W = 110
const CELL_H = 30
const GUTTER_W = 48

export interface GridProps {
  model: SheetModel
  editable: boolean
  /** Bumped by the caller whenever the shared document changes. */
  revision: number
  onEdit?: () => void
}

export default function Grid({ model, editable, revision, onEdit }: GridProps) {
  const [selected, setSelected] = useState<Address>({ row: 0, col: 0 })
  const [editing, setEditing] = useState(false)
  const [draft, setDraft] = useState('')
  const inputRef = useRef<TextInput>(null)

  // A fresh evaluator per revision. Keeping one alive would show stale numbers
  // after an edit, and invalidating it correctly means tracking the dependency
  // graph — a real feature, and an explicit cut rather than something to
  // half-build.
  const evaluator = useMemo(() => new Evaluator(sourceOf(model)), [model, revision])

  const commit = useCallback(
    (value: string) => {
      if (!editable) return
      model.set(selected.row, selected.col, value)
      setEditing(false)
      onEdit?.()
    },
    [editable, model, selected, onEdit],
  )

  const beginEdit = useCallback(() => {
    if (!editable) return
    setDraft(model.get(selected.row, selected.col))
    setEditing(true)
    // Focus after the input mounts; focusing in the same tick is a no-op.
    setTimeout(() => inputRef.current?.focus(), 0)
  }, [editable, model, selected])

  useEffect(() => {
    setEditing(false)
  }, [selected.row, selected.col])

  const rows = useMemo(() => range(VIEW_ROWS), [])
  const cols = useMemo(() => range(VIEW_COLS), [])

  const selectedInput = model.get(selected.row, selected.col)
  const selectedValue = evaluator.value(selected.row, selected.col)

  return (
    <View style={styles.root}>
      {/* The formula bar. It shows the RAW input, always — that is the one
          place a person can see and fix what a cell actually contains. */}
      <View style={styles.formulaBar}>
        <Text style={styles.address}>{addressName(selected)}</Text>
        <TextInput
          style={[styles.formulaInput, !editable && styles.readOnly]}
          value={editing ? draft : selectedInput}
          onChangeText={(t) => {
            setDraft(t)
            if (!editing) setEditing(true)
          }}
          onFocus={beginEdit}
          onSubmitEditing={() => commit(draft)}
          onBlur={() => editing && commit(draft)}
          editable={editable}
          placeholder={editable ? 'Value or =formula' : 'Read only'}
          placeholderTextColor={theme.textFaint}
          accessibilityLabel={`Contents of ${addressName(selected)}`}
        />
        <Text style={[styles.result, isError(selectedValue) && styles.errorText]}>
          {display(selectedValue)}
        </Text>
      </View>

      <ScrollView horizontal contentContainerStyle={styles.hScroll}>
        <View>
          {/* Column headers */}
          <View style={styles.row}>
            <View style={[styles.gutter, styles.headerCell]} />
            {cols.map((c) => (
              <View key={c} style={[styles.cell, styles.headerCell]}>
                <Text style={styles.headerText}>{columnName(c)}</Text>
              </View>
            ))}
          </View>

          <ScrollView style={styles.vScroll}>
            {rows.map((r) => (
              <View key={r} style={styles.row}>
                <View style={[styles.gutter, styles.headerCell]}>
                  <Text style={styles.headerText}>{r + 1}</Text>
                </View>
                {cols.map((c) => {
                  const isSelected = selected.row === r && selected.col === c
                  const value = evaluator.value(r, c)
                  return (
                    <Pressable
                      key={c}
                      onPress={() => setSelected({ row: r, col: c })}
                      onLongPress={beginEdit}
                      style={[styles.cell, isSelected && styles.selectedCell]}
                      accessibilityRole="button"
                      accessibilityLabel={`${addressName({ row: r, col: c })}, ${display(value)}`}
                    >
                      <Text
                        numberOfLines={1}
                        style={[
                          styles.cellText,
                          typeof value === 'number' && styles.numeric,
                          isError(value) && styles.errorText,
                        ]}
                      >
                        {display(value)}
                      </Text>
                    </Pressable>
                  )
                })}
              </View>
            ))}
          </ScrollView>
        </View>
      </ScrollView>
    </View>
  )
}

function range(n: number): number[] {
  return Array.from({ length: n }, (_, i) => i)
}

const styles = StyleSheet.create({
  root: { flex: 1 },
  formulaBar: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: space.sm,
    paddingHorizontal: space.sm,
    paddingVertical: 6,
    borderBottomWidth: StyleSheet.hairlineWidth,
    borderBottomColor: theme.border,
    backgroundColor: theme.surface,
  },
  address: { color: theme.textMuted, fontSize: 13, width: 56, fontVariant: ['tabular-nums'] },
  formulaInput: {
    flex: 1,
    color: theme.text,
    fontSize: 14,
    paddingVertical: 6,
    paddingHorizontal: space.sm,
    backgroundColor: theme.bg,
    borderRadius: 6,
  },
  readOnly: { opacity: 0.6 },
  result: { color: theme.textMuted, fontSize: 13, minWidth: 70, textAlign: 'right' },

  hScroll: { paddingBottom: space.md },
  vScroll: { maxHeight: VIEW_ROWS * CELL_H },
  row: { flexDirection: 'row' },
  gutter: {
    width: GUTTER_W,
    height: CELL_H,
    alignItems: 'center',
    justifyContent: 'center',
  },
  cell: {
    width: CELL_W,
    height: CELL_H,
    paddingHorizontal: 6,
    justifyContent: 'center',
    borderRightWidth: StyleSheet.hairlineWidth,
    borderBottomWidth: StyleSheet.hairlineWidth,
    borderColor: theme.border,
  },
  headerCell: { backgroundColor: theme.surfaceAlt, alignItems: 'center' },
  headerText: { color: theme.textMuted, fontSize: 11, fontWeight: '600' },
  selectedCell: { borderWidth: 2, borderColor: theme.accent },
  cellText: { color: theme.body, fontSize: 13 },
  numeric: { textAlign: 'right', fontVariant: ['tabular-nums'] },
  errorText: { color: theme.danger },
})
