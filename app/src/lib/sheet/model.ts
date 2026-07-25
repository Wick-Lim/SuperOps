import * as Y from 'yjs'

/**
 * The spreadsheet's shared state.
 *
 * ONE DECISION SHAPES EVERYTHING HERE: the CRDT stores only what a person
 * TYPED. Computed values are derived on every client from the same inputs and
 * are never stored, never sent and never merged.
 *
 * The alternative — storing computed values alongside the formulas — is the
 * obvious design and it is a divergence machine. Two clients that evaluate at
 * different moments write different numbers into the same cell; the CRDT
 * resolves that by picking one, and the sheet now shows a number that no
 * formula produces. There is no merge rule that fixes it, because the conflict
 * is not about the data, it is about *when* each client looked.
 *
 * Deriving instead makes convergence free: the inputs converge because Yjs
 * converges them, and equal inputs give equal outputs because evaluation is
 * pure. That purity is a requirement, not a nicety, and it is why there is no
 * NOW(), no TODAY() and no RAND() — see engine.ts.
 */

/** A cell address, zero-based. */
export interface Address {
  row: number
  col: number
}

/** What a person typed into a cell. `null` is an empty cell, not a zero. */
export type CellInput = string

/** The Y.Map key for a cell. Row-major, colon-separated, so the key is stable
 * and sorts predictably in a debugger. */
export function cellKey(row: number, col: number): string {
  return `${row}:${col}`
}

export function parseCellKey(key: string): Address | null {
  const [r, c] = key.split(':')
  const row = Number(r)
  const col = Number(c)
  if (!Number.isInteger(row) || !Number.isInteger(col) || row < 0 || col < 0) return null
  return { row, col }
}

/** Spreadsheet limits. Deliberately smaller than "as many as fit": a sheet is
 * the CRDT log of every edit ever made to it, and a million-cell paste is a
 * log nobody can compact. */
export const MAX_ROWS = 5000
export const MAX_COLS = 200

/**
 * A1 notation, which is what people type and what formulas contain.
 *
 * Columns are bijective base-26: A..Z, AA..AZ, BA.. — NOT plain base-26, which
 * would make A and AA the same column because there is no zero digit.
 */
export function columnName(col: number): string {
  let n = col + 1
  let out = ''
  while (n > 0) {
    const rem = (n - 1) % 26
    out = String.fromCharCode(65 + rem) + out
    n = Math.floor((n - 1) / 26)
  }
  return out
}

export function columnIndex(name: string): number {
  let n = 0
  for (const ch of name.toUpperCase()) {
    const v = ch.charCodeAt(0) - 64
    if (v < 1 || v > 26) return -1
    n = n * 26 + v
  }
  return n - 1
}

export function addressName(a: Address): string {
  return `${columnName(a.col)}${a.row + 1}`
}

/** Parses "B7" or "$B$7". The dollars are accepted and ignored: this sheet has
 * no fill-down, so absolute and relative references cannot differ yet, and
 * refusing them would reject formulas pasted from elsewhere for no benefit. */
export function parseAddress(text: string): Address | null {
  const m = /^\$?([A-Za-z]{1,3})\$?(\d{1,7})$/.exec(text.trim())
  if (!m) return null
  const col = columnIndex(m[1])
  const row = Number(m[2]) - 1
  if (col < 0 || row < 0 || row >= MAX_ROWS || col >= MAX_COLS) return null
  return { row, col }
}

/**
 * The document's shared structures.
 *
 * `cells` is a flat Y.Map rather than a Y.Array of rows, because a spreadsheet
 * is sparse and because inserting a row into a Y.Array of Y.Arrays rewrites
 * every reference in every formula below it. Row insertion is a cut; when it
 * lands it will be an explicit rewrite pass, which is honest about its cost.
 */
export class SheetModel {
  readonly doc: Y.Doc
  readonly cells: Y.Map<CellInput>
  readonly meta: Y.Map<unknown>

  constructor(doc: Y.Doc) {
    this.doc = doc
    this.cells = doc.getMap<CellInput>('cells')
    this.meta = doc.getMap<unknown>('meta')
  }

  get(row: number, col: number): CellInput {
    return this.cells.get(cellKey(row, col)) ?? ''
  }

  /**
   * Writes what the person typed.
   *
   * An empty string DELETES the entry rather than storing "". A sheet whose
   * every visited cell holds an empty string is a sheet whose log grows every
   * time somebody arrows across it.
   */
  set(row: number, col: number, input: CellInput, origin?: unknown) {
    if (row < 0 || col < 0 || row >= MAX_ROWS || col >= MAX_COLS) return
    const key = cellKey(row, col)
    this.doc.transact(() => {
      if (input === '') this.cells.delete(key)
      else this.cells.set(key, input)
    }, origin)
  }

  /** Every non-empty cell. */
  entries(): { address: Address; input: CellInput }[] {
    const out: { address: Address; input: CellInput }[] = []
    this.cells.forEach((input, key) => {
      const address = parseCellKey(key)
      if (address && input !== '') out.push({ address, input })
    })
    out.sort((a, b) => a.address.row - b.address.row || a.address.col - b.address.col)
    return out
  }

  /** The bounding box of the used range, for rendering and for export. */
  bounds(): { rows: number; cols: number } {
    let rows = 0
    let cols = 0
    this.cells.forEach((_input, key) => {
      const a = parseCellKey(key)
      if (!a) return
      if (a.row + 1 > rows) rows = a.row + 1
      if (a.col + 1 > cols) cols = a.col + 1
    })
    return { rows, cols }
  }
}
