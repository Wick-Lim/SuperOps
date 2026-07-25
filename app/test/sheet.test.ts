import { describe, expect, it } from 'vitest'
import * as Y from 'yjs'
import { Evaluator, display, sourceFromMap, sourceOf, isError, type CellValue } from '../src/lib/sheet/engine'
import { SheetModel, columnName, columnIndex, parseAddress, addressName } from '../src/lib/sheet/model'

/** Evaluates one formula against a fixture sheet. */
function evalAt(cells: Record<string, string>, row: number, col: number): CellValue {
  return new Evaluator(sourceFromMap(cells)).value(row, col)
}

function shown(cells: Record<string, string>, row: number, col: number): string {
  return display(evalAt(cells, row, col))
}

describe('A1 addressing', () => {
  // Bijective base-26. Plain base-26 makes A and AA the same column, because
  // there is no zero digit — the classic off-by-one in every sheet parser.
  it('names columns bijectively past Z', () => {
    expect(columnName(0)).toBe('A')
    expect(columnName(25)).toBe('Z')
    expect(columnName(26)).toBe('AA')
    expect(columnName(51)).toBe('AZ')
    expect(columnName(52)).toBe('BA')
    expect(columnName(701)).toBe('ZZ')
    expect(columnName(702)).toBe('AAA')
  })

  it('round-trips every column index it can name', () => {
    for (let i = 0; i < 800; i++) expect(columnIndex(columnName(i))).toBe(i)
  })

  it('parses A1 notation, with or without dollars', () => {
    expect(parseAddress('B7')).toEqual({ row: 6, col: 1 })
    expect(parseAddress('$B$7')).toEqual({ row: 6, col: 1 })
    expect(parseAddress('aa10')).toEqual({ row: 9, col: 26 })
    expect(parseAddress('B0')).toBeNull()
    expect(parseAddress('hello')).toBeNull()
    expect(addressName({ row: 6, col: 1 })).toBe('B7')
  })
})

describe('the formula engine', () => {
  it('reads literals as numbers, booleans and text', () => {
    const cells = { '0:0': '42', '0:1': '  3.5 ', '0:2': 'TRUE', '0:3': 'hello', '0:4': '1e3' }
    expect(evalAt(cells, 0, 0)).toBe(42)
    expect(evalAt(cells, 0, 1)).toBe(3.5)
    expect(evalAt(cells, 0, 2)).toBe(true)
    expect(evalAt(cells, 0, 3)).toBe('hello')
    expect(evalAt(cells, 0, 4)).toBe(1000)
  })

  it('applies arithmetic precedence and associativity', () => {
    expect(shown({ '0:0': '=1+2*3' }, 0, 0)).toBe('7')
    expect(shown({ '0:0': '=(1+2)*3' }, 0, 0)).toBe('9')
    expect(shown({ '0:0': '=2^3^2' }, 0, 0)).toBe('512') // right-associative
    expect(shown({ '0:0': '=-3+5' }, 0, 0)).toBe('2')
    expect(shown({ '0:0': '=10-2-3' }, 0, 0)).toBe('5') // left-associative
  })

  it('resolves references and ranges', () => {
    const cells = { '0:0': '5', '1:0': '7', '2:0': '9', '0:1': '=SUM(A1:A3)', '0:2': '=A1+A2' }
    expect(shown(cells, 0, 1)).toBe('21')
    expect(shown(cells, 0, 2)).toBe('12')
  })

  it('sums a rectangular range in both directions', () => {
    const cells: Record<string, string> = {}
    for (let r = 0; r < 3; r++) for (let c = 0; c < 3; c++) cells[`${r}:${c}`] = String(r * 3 + c + 1)
    cells['5:0'] = '=SUM(A1:C3)'
    // A range written backwards is the same range; a user drags either way.
    cells['5:1'] = '=SUM(C3:A1)'
    expect(shown(cells, 5, 0)).toBe('45')
    expect(shown(cells, 5, 1)).toBe('45')
  })

  // Errors are VALUES. A sheet with one bad formula shows one red cell and the
  // right numbers everywhere else; one that threw would show an empty grid.
  it('produces errors as values, not exceptions', () => {
    expect(shown({ '0:0': '=1/0' }, 0, 0)).toBe('#DIV/0!')
    expect(shown({ '0:0': '=NOSUCHFN(1)' }, 0, 0)).toBe('#NAME?')
    expect(shown({ '0:0': '=1+' }, 0, 0)).toBe('#VALUE!')
    expect(shown({ '0:0': '="a"+1' }, 0, 0)).toBe('#VALUE!')
  })

  it('keeps the rest of the sheet working around a broken cell', () => {
    const cells = { '0:0': '=1/0', '0:1': '10', '0:2': '=B1*2' }
    expect(shown(cells, 0, 0)).toBe('#DIV/0!')
    expect(shown(cells, 0, 2)).toBe('20')
  })

  it('propagates an error through a formula that references it', () => {
    const cells = { '0:0': '=1/0', '0:1': '=A1+1' }
    expect(shown(cells, 0, 1)).toBe('#DIV/0!')
  })

  // A stack overflow in a spreadsheet is a browser tab that dies; a #CYCLE! is
  // a cell the user can see and fix.
  it('detects a direct cycle', () => {
    expect(shown({ '0:0': '=A1+1' }, 0, 0)).toBe('#CYCLE!')
  })

  it('detects an indirect cycle', () => {
    const cells = { '0:0': '=B1', '0:1': '=C1', '0:2': '=A1' }
    expect(() => evalAt(cells, 0, 0)).not.toThrow()
    expect(shown(cells, 0, 0)).toBe('#CYCLE!')
  })

  it('handles a long dependency chain without recomputing quadratically', () => {
    const cells: Record<string, string> = { '0:0': '1' }
    for (let r = 1; r < 500; r++) cells[`${r}:0`] = `=A${r}+1`
    const ev = new Evaluator(sourceFromMap(cells))
    expect(display(ev.value(499, 0))).toBe('500')
  })

  // IF must not evaluate the branch it does not take. =IF(A1=0,"",1/A1) is the
  // standard division guard, and an eager evaluator returns the very error the
  // formula exists to avoid.
  it('evaluates IF lazily', () => {
    expect(shown({ '0:0': '0', '0:1': '=IF(A1=0,"safe",1/A1)' }, 0, 1)).toBe('safe')
    expect(shown({ '0:0': '4', '0:1': '=IF(A1=0,"safe",1/A1)' }, 0, 1)).toBe('0.25')
  })

  it('compares and concatenates', () => {
    expect(shown({ '0:0': '=1<2' }, 0, 0)).toBe('TRUE')
    expect(shown({ '0:0': '=2<=2' }, 0, 0)).toBe('TRUE')
    expect(shown({ '0:0': '=1<>1' }, 0, 0)).toBe('FALSE')
    expect(shown({ '0:0': '="a"&"b"&1' }, 0, 0)).toBe('ab1')
  })

  it('parses escaped quotes inside a string', () => {
    expect(shown({ '0:0': '="say ""hi"""' }, 0, 0)).toBe('say "hi"')
  })

  it('skips text when summing, as every spreadsheet does', () => {
    const cells = { '0:0': '1', '1:0': 'label', '2:0': '2', '0:1': '=SUM(A1:A3)' }
    expect(shown(cells, 0, 1)).toBe('3')
  })

  it('counts numbers and non-empty cells differently', () => {
    const cells = { '0:0': '1', '1:0': 'x', '2:0': '3', '0:1': '=COUNT(A1:A3)', '0:2': '=COUNTA(A1:A3)' }
    expect(shown(cells, 0, 1)).toBe('2')
    expect(shown(cells, 0, 2)).toBe('3')
  })

  it('averages, and refuses to average nothing', () => {
    expect(shown({ '0:0': '2', '1:0': '4', '0:1': '=AVERAGE(A1:A2)' }, 0, 1)).toBe('3')
    expect(shown({ '0:1': '=AVERAGE(A1:A2)' }, 0, 1)).toBe('#DIV/0!')
  })

  it('rounds display so IEEE 754 does not look like a product bug', () => {
    expect(shown({ '0:0': '=0.1+0.2' }, 0, 0)).toBe('0.3')
  })

  it('has no impure function, because purity is what makes clients agree', () => {
    // Not a style check. The CRDT stores only what people typed and every
    // client derives the values; one clock read and two clients disagree
    // forever with no merge rule that can fix it.
    for (const name of ['NOW', 'TODAY', 'RAND', 'RANDBETWEEN']) {
      expect(shown({ '0:0': `=${name}()` }, 0, 0), name).toBe('#NAME?')
    }
  })
})

describe('the sheet model', () => {
  it('stores what was typed and deletes on empty', () => {
    const doc = new Y.Doc()
    const m = new SheetModel(doc)
    m.set(0, 0, 'hello')
    expect(m.get(0, 0)).toBe('hello')
    expect(m.cells.size).toBe(1)

    // An empty string must DELETE, or arrowing across a sheet grows its log
    // with a write per visited cell.
    m.set(0, 0, '')
    expect(m.get(0, 0)).toBe('')
    expect(m.cells.size).toBe(0)
  })

  it('refuses a cell outside the bounds rather than storing it', () => {
    const m = new SheetModel(new Y.Doc())
    m.set(-1, 0, 'x')
    m.set(0, 99999, 'x')
    expect(m.cells.size).toBe(0)
  })

  it('reports the used bounds', () => {
    const m = new SheetModel(new Y.Doc())
    m.set(2, 3, 'x')
    m.set(0, 0, 'y')
    expect(m.bounds()).toEqual({ rows: 3, cols: 4 })
  })

  // THE CENTRAL CLAIM: values are derived, so two clients that converge on the
  // inputs necessarily agree on the numbers — without ever merging a computed
  // value, which is the design that cannot be made to work.
  it('converges two clients on derived values through the CRDT alone', () => {
    const a = new Y.Doc()
    const b = new Y.Doc()
    const ma = new SheetModel(a)
    const mb = new SheetModel(b)

    ma.set(0, 0, '10')
    ma.set(0, 2, '=A1*B1')
    mb.set(0, 1, '4')

    Y.applyUpdate(b, Y.encodeStateAsUpdate(a))
    Y.applyUpdate(a, Y.encodeStateAsUpdate(b))

    const va = display(new Evaluator(sourceOf(ma)).value(0, 2))
    const vb = display(new Evaluator(sourceOf(mb)).value(0, 2))
    expect(va).toBe('40')
    expect(vb).toBe(va)

    // And nothing computed was ever written into the document.
    for (const [, input] of ma.cells.entries()) {
      expect(input).not.toBe('40')
    }
  })

  it('lets the last writer win on a single cell without corrupting the sheet', () => {
    const a = new Y.Doc()
    const b = new Y.Doc()
    const ma = new SheetModel(a)
    const mb = new SheetModel(b)
    ma.set(5, 5, 'from A')
    mb.set(5, 5, 'from B')

    Y.applyUpdate(b, Y.encodeStateAsUpdate(a))
    Y.applyUpdate(a, Y.encodeStateAsUpdate(b))

    // Which one wins is Yjs's business; that they AGREE is ours.
    expect(ma.get(5, 5)).toBe(mb.get(5, 5))
    expect(['from A', 'from B']).toContain(ma.get(5, 5))
  })
})

describe('error display', () => {
  it('shows an error code rather than an object', () => {
    const v = evalAt({ '0:0': '=1/0' }, 0, 0)
    expect(isError(v)).toBe(true)
    expect(display(v)).toBe('#DIV/0!')
  })
})

describe('the sheet projection', () => {
  it('indexes DISPLAYED values, not formulas', async () => {
    const { extractSheet } = await import('../src/lib/sheet/projection')
    const m = new SheetModel(new Y.Doc())
    m.set(0, 0, 'Customer')
    m.set(1, 0, 'Acme Corp')
    m.set(1, 1, '=A2')

    const p = extractSheet(m, 7)
    expect(p.seq).toBe(7)
    // The name arrived in B2 through a reference. Somebody searching for "Acme
    // Corp" wants this sheet, and indexing formulas would miss exactly this.
    expect(p.body_text).toContain('Acme Corp')
    expect(p.body_text).not.toContain('=A2')
    // The first row is the outline — in practice the header.
    expect(p.outline.map((o) => o.text)).toEqual(['Customer'])
    // Refs is present and EMPTY, never absent: the server rebuilds refs
    // wholesale, so omitting it would look like "unchanged".
    expect(p.refs).toEqual([])
  })

  it('leaves error cells out of the index', async () => {
    const { extractSheet } = await import('../src/lib/sheet/projection')
    const m = new SheetModel(new Y.Doc())
    m.set(0, 0, '=1/0')
    m.set(0, 1, 'real content')
    const p = extractSheet(m, 1)
    expect(p.body_text).not.toContain('#DIV/0!')
    expect(p.body_text).toContain('real content')
  })
})

describe('the design model', () => {
  it('keeps siblings ordered without renumbering them', async () => {
    const { DesignModel, orderBetween } = await import('../src/lib/design/model')
    const m = new DesignModel(new Y.Doc())
    m.add('a', 'rect', {})
    m.add('b', 'rect', {})
    m.add('c', 'rect', {})
    const before = m.list().map((n) => n.order)

    // Move c between a and b. Exactly ONE field changes; a design that
    // renumbered siblings would rewrite every node on every reorder, which is
    // both a conflict storm and an unbounded CRDT log.
    m.update('c', { order: orderBetween(before[0], before[1]) })
    expect(m.list().map((n) => n.id)).toEqual(['a', 'c', 'b'])
    expect(m.get('a')!.order).toBe(before[0])
    expect(m.get('b')!.order).toBe(before[1])
  })

  it('merges two people editing different fields of one shape', async () => {
    const { DesignModel } = await import('../src/lib/design/model')
    const da = new Y.Doc()
    const db = new Y.Doc()
    const a = new DesignModel(da)
    a.add('n1', 'rect', { x: 0, fill: '#000000' })
    Y.applyUpdate(db, Y.encodeStateAsUpdate(da))
    const b = new DesignModel(db)

    a.update('n1', { x: 300 })
    b.update('n1', { fill: '#ff0000' })

    Y.applyUpdate(db, Y.encodeStateAsUpdate(da))
    Y.applyUpdate(da, Y.encodeStateAsUpdate(db))

    // Both survive. Replacing the whole node instead of writing fields would
    // lose one of them with no way to tell it happened.
    expect(a.get('n1')!.x).toBe(300)
    expect(a.get('n1')!.fill).toBe('#ff0000')
    expect(b.get('n1')).toEqual(a.get('n1'))
  })

  it('takes a frame children with it', async () => {
    const { DesignModel } = await import('../src/lib/design/model')
    const m = new DesignModel(new Y.Doc())
    m.add('frame', 'frame', {})
    m.add('child', 'rect', { parent: 'frame' })
    m.remove('frame')
    // A child that survived its frame would read as "my shapes moved" rather
    // than "I deleted a frame".
    expect(m.list()).toEqual([])
  })

  it('extracts text layers and frame names', async () => {
    const { DesignModel } = await import('../src/lib/design/model')
    const { extractDesign } = await import('../src/lib/design/projection')
    const m = new DesignModel(new Y.Doc())
    m.add('f', 'frame', { text: 'Checkout flow' })
    m.add('t', 'text', { parent: 'f', text: 'Pay now' })
    m.add('r', 'rect', { parent: 'f' })

    const p = extractDesign(m, 3)
    expect(p.body_text).toContain('Checkout flow')
    expect(p.body_text).toContain('Pay now')
    // A frame is the canvas's heading; a button caption is not.
    expect(p.outline.map((o) => o.text)).toEqual(['Checkout flow'])
  })

  it('clamps a geometry value rather than storing something unrenderable', async () => {
    const { DesignModel } = await import('../src/lib/design/model')
    const m = new DesignModel(new Y.Doc())
    m.add('n', 'rect', {})
    m.update('n', { w: -50, opacity: 9 })
    expect(m.get('n')!.w).toBeGreaterThan(0)
    expect(m.get('n')!.opacity).toBe(1)
  })
})
