import type { Projection } from '../../editor/projection'
import { Evaluator, display, isError, sourceOf } from './engine'
import { addressName, type SheetModel } from './model'

/**
 * The spreadsheet's extractor. THIRTY LINES, and that is the whole of what this
 * editor adds to the projection pipeline.
 *
 * WHAT GOES IN THE BODY IS A REAL DECISION, not an implementation detail, and
 * this is the answer: the DISPLAYED values, not the formulas.
 *
 * Somebody searching for "Acme Corp" wants the sheet that shows Acme Corp. They
 * do not want the sheet whose formula happens to contain the text, and they
 * definitely want the one where the name arrived through a reference — which is
 * exactly the case indexing formulas would miss and indexing values catches.
 * Formulas are the sheet's source code; a code search over spreadsheets is a
 * different feature with a different query.
 *
 * The cost is that the body is only as fresh as the last person to have the
 * sheet open, which is true of every projection and is the freshness floor the
 * whole design accepts.
 */

/** The projection is bounded at 1 MiB, and a large sheet exceeds that long
 * before it exceeds a person's patience. Truncation is by CELL COUNT first so
 * the body ends at a cell boundary rather than mid-number, which would index a
 * value that does not exist. */
const MAX_CELLS = 20_000

export const SHEET_SCHEMA_VERSION = 1

export function extractSheet(model: SheetModel, seq: number): Projection {
  const evaluator = new Evaluator(sourceOf(model))
  const entries = model.entries()

  const parts: string[] = []
  const outline: Projection['outline'] = []
  let count = 0

  for (const { address } of entries) {
    if (count >= MAX_CELLS) break
    const value = evaluator.value(address.row, address.col)
    // An error cell contributes nothing. "#DIV/0!" in the search index is
    // noise at best, and at worst it makes every broken sheet a hit for the
    // same query.
    if (isError(value) || value === '') continue
    const text = display(value)
    if (text.trim() === '') continue
    parts.push(text)
    count++

    // The first row is the outline: in practice it is the header, and it is
    // what a link preview should show. Levels are all 1 because a sheet has no
    // hierarchy — pretending otherwise would put fake structure in the
    // document's table of contents.
    if (address.row === 0 && outline.length < 50) {
      outline.push({ block_id: addressName(address), level: 1, text: text.slice(0, 512) })
    }
  }

  return {
    seq,
    schema_version: SHEET_SCHEMA_VERSION,
    body_text: parts.join(' ').slice(0, 1 << 20),
    outline,
    // A sheet has no embeds in v1, so it references nothing. The field is
    // present and empty rather than absent, because the server rebuilds refs
    // WHOLESALE from each projection — omitting it would be indistinguishable
    // from "no change" and stale backlinks would survive forever.
    refs: [],
  }
}
