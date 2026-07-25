import { parseAddress, addressName, type Address, type SheetModel } from './model'

/**
 * The formula engine.
 *
 * PURE, BY REQUIREMENT. The CRDT stores only what people typed; every client
 * derives the displayed values from those inputs. That is what makes two
 * clients agree without the sheet ever merging a computed number — but it holds
 * only while evaluation is a function of the inputs alone.
 *
 * So there is no NOW(), no TODAY(), no RAND(), and no reference to anything
 * outside the sheet. Those are not missing features to add later; adding one
 * breaks the property the whole design rests on, and the honest way to have
 * "today" in a sheet is a cell somebody typed a date into.
 *
 * Errors are VALUES, not exceptions. A spreadsheet with one bad formula shows
 * #DIV/0! in one cell and the right numbers everywhere else; one that threw
 * would show an empty grid, and the person would have no idea which cell to
 * fix.
 */

export type CellValue = number | string | boolean | CellError

export class CellError {
  constructor(readonly code: ErrorCode, readonly detail?: string) {}
  toString() {
    return this.code
  }
}

export type ErrorCode = '#DIV/0!' | '#VALUE!' | '#REF!' | '#NAME?' | '#CYCLE!' | '#N/A'

/** Accepts `unknown` rather than `CellValue` so it also narrows the
 * `number[] | CellError` a coercion helper returns — otherwise every call site
 * would need its own `instanceof`, which is how one of them ends up missing. */
export const isError = (v: unknown): v is CellError => v instanceof CellError

// ---------------------------------------------------------------------------
// Tokenizer
// ---------------------------------------------------------------------------

type Token =
  | { k: 'num'; v: number }
  | { k: 'str'; v: string }
  | { k: 'ref'; v: Address }
  | { k: 'range'; from: Address; to: Address }
  | { k: 'name'; v: string }
  | { k: 'op'; v: string }
  | { k: 'lparen' }
  | { k: 'rparen' }
  | { k: 'comma' }

function tokenize(src: string): Token[] | CellError {
  const out: Token[] = []
  let i = 0
  while (i < src.length) {
    const ch = src[i]
    if (ch === ' ' || ch === '\t' || ch === '\n') {
      i++
      continue
    }
    if (ch === '(') {
      out.push({ k: 'lparen' })
      i++
      continue
    }
    if (ch === ')') {
      out.push({ k: 'rparen' })
      i++
      continue
    }
    if (ch === ',' || ch === ';') {
      out.push({ k: 'comma' })
      i++
      continue
    }
    if (ch === '"') {
      let j = i + 1
      let s = ''
      while (j < src.length) {
        if (src[j] === '"') {
          // "" is an escaped quote, which is the spreadsheet convention.
          if (src[j + 1] === '"') {
            s += '"'
            j += 2
            continue
          }
          break
        }
        s += src[j++]
      }
      if (j >= src.length) return new CellError('#VALUE!', 'unterminated string')
      out.push({ k: 'str', v: s })
      i = j + 1
      continue
    }
    // Two-character comparisons first, or "<=" tokenizes as "<" then "=".
    const two = src.slice(i, i + 2)
    if (two === '<=' || two === '>=' || two === '<>') {
      out.push({ k: 'op', v: two })
      i += 2
      continue
    }
    if ('+-*/^&<>='.includes(ch)) {
      out.push({ k: 'op', v: ch })
      i++
      continue
    }
    if (/[0-9.]/.test(ch)) {
      const m = /^\d*\.?\d+(?:[eE][+-]?\d+)?/.exec(src.slice(i))
      if (!m) return new CellError('#VALUE!', 'malformed number')
      out.push({ k: 'num', v: Number(m[0]) })
      i += m[0].length
      continue
    }
    if (/[A-Za-z_$]/.test(ch)) {
      const m = /^[$A-Za-z_][A-Za-z0-9_.$]*/.exec(src.slice(i))!
      const word = m[0]
      i += word.length
      // A range is two references joined by a colon, and it must be recognised
      // here rather than as an operator: A1:B2 is one operand.
      if (src[i] === ':') {
        const m2 = /^[$A-Za-z_][A-Za-z0-9_$]*/.exec(src.slice(i + 1))
        const from = parseAddress(word)
        const to = m2 ? parseAddress(m2[0]) : null
        if (from && to && m2) {
          out.push({ k: 'range', from, to })
          i += 1 + m2[0].length
          continue
        }
      }
      const addr = parseAddress(word)
      if (addr) out.push({ k: 'ref', v: addr })
      else out.push({ k: 'name', v: word.toUpperCase() })
      continue
    }
    return new CellError('#VALUE!', `unexpected character ${ch}`)
  }
  return out
}

// ---------------------------------------------------------------------------
// Parser — precedence climbing
// ---------------------------------------------------------------------------

type Ast =
  | { k: 'num'; v: number }
  | { k: 'str'; v: string }
  | { k: 'ref'; v: Address }
  | { k: 'range'; from: Address; to: Address }
  | { k: 'call'; name: string; args: Ast[] }
  | { k: 'bin'; op: string; l: Ast; r: Ast }
  | { k: 'neg'; e: Ast }

const PRECEDENCE: Record<string, number> = {
  '=': 1, '<': 1, '>': 1, '<=': 1, '>=': 1, '<>': 1,
  '&': 2,
  '+': 3, '-': 3,
  '*': 4, '/': 4,
  '^': 5,
}

function parse(tokens: Token[]): Ast | CellError {
  let pos = 0
  const peek = () => tokens[pos]

  function primary(): Ast | CellError {
    const t = tokens[pos]
    if (!t) return new CellError('#VALUE!', 'unexpected end of formula')
    pos++
    switch (t.k) {
      case 'num':
        return { k: 'num', v: t.v }
      case 'str':
        return { k: 'str', v: t.v }
      case 'ref':
        return { k: 'ref', v: t.v }
      case 'range':
        return { k: 'range', from: t.from, to: t.to }
      case 'lparen': {
        const e = expr(0)
        if (e instanceof CellError) return e
        if (peek()?.k !== 'rparen') return new CellError('#VALUE!', 'missing )')
        pos++
        return e
      }
      case 'op':
        if (t.v === '-') {
          const e = primary()
          return e instanceof CellError ? e : { k: 'neg', e }
        }
        if (t.v === '+') return primary()
        return new CellError('#VALUE!', `unexpected ${t.v}`)
      case 'name': {
        if (peek()?.k !== 'lparen') {
          if (t.v === 'TRUE') return { k: 'num', v: 1 }
          if (t.v === 'FALSE') return { k: 'num', v: 0 }
          return new CellError('#NAME?', t.v)
        }
        pos++
        const args: Ast[] = []
        if (peek()?.k !== 'rparen') {
          for (;;) {
            const a = expr(0)
            if (a instanceof CellError) return a
            args.push(a)
            if (peek()?.k === 'comma') {
              pos++
              continue
            }
            break
          }
        }
        if (peek()?.k !== 'rparen') return new CellError('#VALUE!', 'missing ) after arguments')
        pos++
        return { k: 'call', name: t.v, args }
      }
      default:
        return new CellError('#VALUE!', 'unexpected token')
    }
  }

  function expr(min: number): Ast | CellError {
    let left = primary()
    if (left instanceof CellError) return left
    for (;;) {
      const t = peek()
      if (!t || t.k !== 'op') break
      const prec = PRECEDENCE[t.v]
      if (prec === undefined || prec < min) break
      pos++
      // ^ is right-associative; everything else is left.
      const right = expr(t.v === '^' ? prec : prec + 1)
      if (right instanceof CellError) return right
      left = { k: 'bin', op: t.v, l: left, r: right }
    }
    return left
  }

  const out = expr(0)
  if (out instanceof CellError) return out
  if (pos !== tokens.length) return new CellError('#VALUE!', 'trailing input')
  return out
}

// ---------------------------------------------------------------------------
// Evaluation
// ---------------------------------------------------------------------------

/** Reads a cell's raw input. Kept as an interface so the engine can be tested
 * without a Y.Doc, and so a future sparse cache can slot in unchanged. */
export interface CellSource {
  input(row: number, col: number): string
}

export function sourceOf(model: SheetModel): CellSource {
  return { input: (row, col) => model.get(row, col) }
}

/** A plain-object source, for tests and for the projection extractor. */
export function sourceFromMap(cells: Record<string, string>): CellSource {
  return { input: (row, col) => cells[`${row}:${col}`] ?? '' }
}

/**
 * Evaluates every referenced cell, memoised, with cycle detection.
 *
 * The evaluator is created per render pass rather than kept alive: a cache that
 * outlived an edit would show stale numbers, and invalidating it correctly
 * means tracking the dependency graph — which is a real feature and an explicit
 * cut, not something to half-build.
 */
export class Evaluator {
  private cache = new Map<string, CellValue>()
  private visiting = new Set<string>()

  constructor(private readonly source: CellSource) {}

  /** The value shown in a cell. */
  value(row: number, col: number): CellValue {
    const key = `${row}:${col}`
    const cached = this.cache.get(key)
    if (cached !== undefined) return cached

    if (this.visiting.has(key)) {
      // A formula that depends on itself. Returning an error rather than
      // recursing is the difference between one red cell and a blown stack.
      return new CellError('#CYCLE!', addressName({ row, col }))
    }
    this.visiting.add(key)
    let out: CellValue
    try {
      out = this.compute(row, col)
    } finally {
      this.visiting.delete(key)
    }
    this.cache.set(key, out)
    return out
  }

  private compute(row: number, col: number): CellValue {
    const raw = this.source.input(row, col)
    if (raw === '') return ''
    if (!raw.startsWith('=')) return literal(raw)

    const tokens = tokenize(raw.slice(1))
    if (tokens instanceof CellError) return tokens
    const ast = parse(tokens)
    if (ast instanceof CellError) return ast
    return this.evalNode(ast)
  }

  private evalNode(node: Ast): CellValue {
    switch (node.k) {
      case 'num':
        return node.v
      case 'str':
        return node.v
      case 'ref':
        return this.value(node.v.row, node.v.col)
      case 'range':
        // A bare range outside a function has no single value. SUM(A1:A3) is
        // handled by the function; A1:A3 alone is a mistake worth naming.
        return new CellError('#VALUE!', 'a range needs a function')
      case 'neg': {
        const v = this.evalNode(node.e)
        if (isError(v)) return v
        const n = toNumber(v)
        return isError(n) ? n : -n
      }
      case 'bin':
        return this.evalBinary(node)
      case 'call':
        return this.evalCall(node)
    }
  }

  private evalBinary(node: { op: string; l: Ast; r: Ast }): CellValue {
    const l = this.evalNode(node.l)
    if (isError(l)) return l
    const r = this.evalNode(node.r)
    if (isError(r)) return r

    if (node.op === '&') return `${display(l)}${display(r)}`

    if ('=<>'.includes(node.op[0]) && !'+-*/^'.includes(node.op)) {
      const cmp = compare(l, r)
      switch (node.op) {
        case '=': return cmp === 0
        case '<>': return cmp !== 0
        case '<': return cmp < 0
        case '<=': return cmp <= 0
        case '>': return cmp > 0
        case '>=': return cmp >= 0
      }
    }

    const a = toNumber(l)
    if (isError(a)) return a
    const b = toNumber(r)
    if (isError(b)) return b
    switch (node.op) {
      case '+': return a + b
      case '-': return a - b
      case '*': return a * b
      case '/':
        // The one error every spreadsheet user recognises, and the reason
        // errors are values: the rest of the sheet keeps working.
        return b === 0 ? new CellError('#DIV/0!') : a / b
      case '^': return Math.pow(a, b)
    }
    return new CellError('#VALUE!', node.op)
  }

  /** Flattens an argument into the values a function sees. A range expands; a
   * scalar is a list of one. */
  private args(node: Ast): CellValue[] {
    if (node.k === 'range') {
      const out: CellValue[] = []
      const r0 = Math.min(node.from.row, node.to.row)
      const r1 = Math.max(node.from.row, node.to.row)
      const c0 = Math.min(node.from.col, node.to.col)
      const c1 = Math.max(node.from.col, node.to.col)
      for (let r = r0; r <= r1; r++) {
        for (let c = c0; c <= c1; c++) out.push(this.value(r, c))
      }
      return out
    }
    return [this.evalNode(node)]
  }

  private evalCall(node: { name: string; args: Ast[] }): CellValue {
    const fn = FUNCTIONS[node.name]
    if (!fn) return new CellError('#NAME?', node.name)
    // IF must not evaluate the branch it does not take: =IF(A1=0,"",1/A1) is
    // the standard way to guard a division, and an eager evaluator would
    // return #DIV/0! from the arm the formula exists to avoid.
    if (node.name === 'IF') {
      if (node.args.length < 2) return new CellError('#VALUE!', 'IF needs a condition')
      const cond = this.evalNode(node.args[0])
      if (isError(cond)) return cond
      const branch = truthy(cond) ? node.args[1] : node.args[2]
      return branch === undefined ? false : this.evalNode(branch)
    }
    const flat: CellValue[] = []
    for (const a of node.args) flat.push(...this.args(a))
    return fn(flat)
  }
}

// ---------------------------------------------------------------------------
// Coercion
// ---------------------------------------------------------------------------

/** What a typed cell means when it is not a formula. */
function literal(raw: string): CellValue {
  const t = raw.trim()
  if (t === '') return ''
  if (/^-?\d*\.?\d+(?:[eE][+-]?\d+)?$/.test(t)) return Number(t)
  if (/^TRUE$/i.test(t)) return true
  if (/^FALSE$/i.test(t)) return false
  return raw
}

function toNumber(v: CellValue): number | CellError {
  if (typeof v === 'number') return v
  if (typeof v === 'boolean') return v ? 1 : 0
  if (v === '') return 0
  if (isError(v)) return v
  const n = Number(String(v).trim())
  return Number.isFinite(n) ? n : new CellError('#VALUE!', String(v))
}

function truthy(v: CellValue): boolean {
  if (typeof v === 'boolean') return v
  if (typeof v === 'number') return v !== 0
  if (isError(v)) return false
  return String(v).trim() !== '' && !/^false$/i.test(String(v))
}

function compare(a: CellValue, b: CellValue): number {
  if (typeof a === 'number' && typeof b === 'number') return a === b ? 0 : a < b ? -1 : 1
  const sa = display(a)
  const sb = display(b)
  return sa === sb ? 0 : sa < sb ? -1 : 1
}

/** The string a value shows as. */
export function display(v: CellValue): string {
  if (isError(v)) return v.code
  if (typeof v === 'boolean') return v ? 'TRUE' : 'FALSE'
  if (typeof v === 'number') {
    if (!Number.isFinite(v)) return '#VALUE!'
    // Floating point makes 0.1+0.2 print as 0.30000000000000004, which in a
    // spreadsheet reads as a bug in the product rather than in IEEE 754.
    const rounded = Math.round(v * 1e10) / 1e10
    return String(rounded)
  }
  return v
}

// ---------------------------------------------------------------------------
// Functions
// ---------------------------------------------------------------------------

const numbersOf = (vals: CellValue[]): number[] | CellError => {
  const out: number[] = []
  for (const v of vals) {
    if (isError(v)) return v
    if (v === '' || typeof v === 'string') continue // text is skipped, as in every sheet
    const n = toNumber(v)
    if (isError(n)) return n
    out.push(n)
  }
  return out
}

type Fn = (args: CellValue[]) => CellValue

/**
 * The function set.
 *
 * Deliberately small and deliberately PURE. Nothing here reads a clock, a
 * random source, a locale or anything outside its arguments — see the header:
 * a single impure function would break the property that makes two clients
 * agree without ever merging a computed value.
 */
const FUNCTIONS: Record<string, Fn> = {
  SUM: (a) => {
    const n = numbersOf(a)
    return isError(n) ? n : n.reduce((x, y) => x + y, 0)
  },
  AVERAGE: (a) => {
    const n = numbersOf(a)
    if (isError(n)) return n
    return n.length === 0 ? new CellError('#DIV/0!') : n.reduce((x, y) => x + y, 0) / n.length
  },
  MIN: (a) => {
    const n = numbersOf(a)
    if (isError(n)) return n
    return n.length === 0 ? 0 : Math.min(...n)
  },
  MAX: (a) => {
    const n = numbersOf(a)
    if (isError(n)) return n
    return n.length === 0 ? 0 : Math.max(...n)
  },
  COUNT: (a) => {
    const n = numbersOf(a)
    return isError(n) ? n : n.length
  },
  COUNTA: (a) => a.filter((v) => v !== '' && !isError(v)).length,
  ABS: (a) => unary(a, Math.abs),
  ROUND: (a) => {
    const n = numbersOf(a)
    if (isError(n)) return n
    const [x, digits = 0] = n
    if (x === undefined) return new CellError('#VALUE!', 'ROUND needs a number')
    const f = Math.pow(10, digits)
    return Math.round(x * f) / f
  },
  SQRT: (a) => unary(a, (x) => (x < 0 ? NaN : Math.sqrt(x))),
  POWER: (a) => {
    const n = numbersOf(a)
    if (isError(n)) return n
    return Math.pow(n[0] ?? 0, n[1] ?? 0)
  },
  CONCAT: (a) => a.map(display).join(''),
  CONCATENATE: (a) => a.map(display).join(''),
  LEN: (a) => display(a[0] ?? '').length,
  UPPER: (a) => display(a[0] ?? '').toUpperCase(),
  LOWER: (a) => display(a[0] ?? '').toLowerCase(),
  TRIM: (a) => display(a[0] ?? '').trim(),
  NOT: (a) => !truthy(a[0] ?? false),
  AND: (a) => a.every(truthy),
  OR: (a) => a.some(truthy),
  // IF is handled in evalCall so its branches stay lazy; this entry exists so
  // the name resolves and an arity mistake is #VALUE! rather than #NAME?.
  IF: () => new CellError('#VALUE!', 'IF'),
}

function unary(args: CellValue[], f: (x: number) => number): CellValue {
  const v = args[0]
  if (v === undefined) return new CellError('#VALUE!', 'missing argument')
  if (isError(v)) return v
  const n = toNumber(v)
  if (isError(n)) return n
  const out = f(n)
  return Number.isFinite(out) ? out : new CellError('#VALUE!')
}

/** The function names, for the client's autocomplete. */
export const FUNCTION_NAMES = Object.keys(FUNCTIONS).sort()
