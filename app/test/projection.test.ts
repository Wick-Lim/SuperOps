import { afterEach, describe, expect, it, vi } from 'vitest'
import { extract, SCHEMA_VERSION, type Node } from '../src/editor/projection'

const doc = (...content: Node[]): Node => ({ type: 'doc', content })
const p = (text: string, blockId = 'b1'): Node => ({
  type: 'paragraph',
  attrs: { blockId },
  content: [{ text }],
})
const h = (level: number, text: string, blockId = 'h1'): Node => ({
  type: 'heading',
  attrs: { blockId, level },
  content: [{ text }],
})

afterEach(() => {
  vi.useRealTimers()
})

describe('the projection extractor', () => {
  it('renders block text with boundaries, not one run-on word', () => {
    const out = extract(doc(p('First paragraph.', 'a'), p('Second paragraph.', 'b')), 3)
    expect(out.seq).toBe(3)
    expect(out.schema_version).toBe(SCHEMA_VERSION)
    expect(out.body_text).toContain('First paragraph.')
    expect(out.body_text).toContain('Second paragraph.')
    // Without a boundary the index sees "paragraph.Second" as one token and
    // searching for "Second" misses the document.
    expect(out.body_text).not.toContain('paragraph.Second')
  })

  it('builds an outline from headings', () => {
    const out = extract(doc(h(1, 'Overview', 'x'), p('body'), h(2, 'Details', 'y')), 1)
    expect(out.outline).toEqual([
      { block_id: 'x', level: 1, text: 'Overview' },
      { block_id: 'y', level: 2, text: 'Details' },
    ])
  })

  // THE SECURITY INVARIANT, as a test rather than a comment.
  //
  // The document body is an opaque blob the server cannot filter, so the only
  // defence against sharing a document that embeds something the reader may not
  // see is that the body never contains anything worth leaking. An embed node
  // contributes a bare {ref_type, ref_id} and NOT its label — the server
  // resolves the name per caller.
  it('never writes an embed label into the body', () => {
    const secret = 'Project Nightingale — acquisition terms.pdf'
    const out = extract(
      doc(
        p('See the attached file:', 'a'),
        {
          type: 'driveEmbed',
          attrs: {
            blockId: 'e1',
            refId: '11111111-1111-1111-1111-111111111111',
            // A client that put the title in the node — which is the obvious
            // thing to do, because it makes the placeholder render instantly —
            // must not have it reach the projection.
            title: secret,
            filename: secret,
          },
        },
        p('Thoughts?', 'b'),
      ),
      1,
    )

    expect(out.body_text).not.toContain('Nightingale')
    expect(out.body_text).not.toContain(secret)
    expect(JSON.stringify(out)).not.toContain('Nightingale')

    // But the reference itself IS there, bare, so backlinks and per-caller
    // resolution work.
    expect(out.refs).toEqual([
      { ref_type: 'file', ref_id: '11111111-1111-1111-1111-111111111111', block_id: 'e1' },
    ])
  })

  it('extracts a mention as a user ref and not as a name', () => {
    const out = extract(
      doc({
        type: 'paragraph',
        attrs: { blockId: 'm' },
        content: [
          { text: 'ping ' },
          {
            type: 'mention',
            attrs: { refId: '22222222-2222-2222-2222-222222222222', label: 'Dana Okonkwo' },
          },
        ],
      }),
      1,
    )
    expect(out.body_text).not.toContain('Dana')
    expect(out.refs).toEqual([
      { ref_type: 'user', ref_id: '22222222-2222-2222-2222-222222222222', block_id: 'm' },
    ])
  })

  it('deduplicates a reference repeated in the same block', () => {
    const embed = (id: string): Node => ({
      type: 'driveEmbed',
      attrs: { blockId: 'same', refId: id },
    })
    const out = extract(doc(embed('33333333-3333-3333-3333-333333333333'), embed('33333333-3333-3333-3333-333333333333')), 1)
    expect(out.refs.length).toBe(1)
  })

  // A block type shipped to web before this extractor knows it must still
  // contribute its text. Otherwise the first schema addition makes half the
  // corpus unsearchable and nothing reports it.
  it('walks an unknown node type instead of dropping it', () => {
    const out = extract(
      doc({
        type: 'calloutBlockFromTheFuture',
        attrs: { blockId: 'f' },
        content: [{ text: 'important information' }],
      }),
      1,
    )
    expect(out.body_text).toContain('important information')
  })

  // The server's limit is octet_length and one emoji is four bytes. Slicing by
  // characters would send a body the database refuses, which a user would
  // experience as a document that quietly stopped being searchable.
  it('truncates the body by BYTES, not by characters', () => {
    const emoji = '🙂'
    const out = extract(doc(p(emoji.repeat(400_000), 'big')), 1)
    const bytes = new TextEncoder().encode(out.body_text).length
    expect(bytes).toBeLessThanOrEqual(1 << 20)
    // And it did not cut mid-codepoint, which would produce a lone surrogate.
    expect(out.body_text).not.toMatch(/[\uD800-\uDBFF]$/)
  })

  it('caps the outline and the ref list rather than sending a rejected projection', () => {
    const headings = Array.from({ length: 700 }, (_, i) => h(1, `H${i}`, `h${i}`))
    const embeds: Node[] = Array.from({ length: 1500 }, (_, i) => ({
      type: 'driveEmbed',
      attrs: { blockId: `e${i}`, refId: `00000000-0000-0000-0000-${String(i).padStart(12, '0')}` },
    }))
    const out = extract(doc(...headings, ...embeds), 1)
    expect(out.outline.length).toBe(500)
    expect(out.refs.length).toBe(1000)
  })

  it('clamps a heading level into the range the server accepts', () => {
    const out = extract(doc(h(9, 'Too deep', 'd'), h(0, 'Too shallow', 's')), 1)
    expect(out.outline.map((e) => e.level)).toEqual([6, 1])
  })

  it('truncates a block id to the 64 bytes the server stores', () => {
    const long = 'x'.repeat(200)
    const out = extract(
      doc({ type: 'driveEmbed', attrs: { blockId: long, refId: '44444444-4444-4444-4444-444444444444' } }),
      1,
    )
    expect(out.refs[0].block_id?.length).toBe(64)
  })
})

// THE SCHEMA AND THE EXTRACTOR MUST NOT DRIFT.
//
// They were two hand-written lists. A node type added to one and not the other
// produces a document whose references are never extracted — so the backlink
// list quietly stops filling, and nothing anywhere reports it. The extractor
// now imports the schema's map; this asserts that it is actually the same
// object, so re-introducing a copy fails here.
describe('the reference vocabulary', () => {
  it('is the schema\'s, not a second copy', async () => {
    const { REF_TYPES } = await import('../src/editor/extensions/refs')
    // Every node the schema defines must be extractable.
    for (const [node, refType] of Object.entries(REF_TYPES)) {
      const out = extract(
        doc({ type: node, attrs: { blockId: 'b', refId: '11111111-1111-1111-1111-111111111111' } }),
        1,
      )
      expect(out.refs, `${node} produced no ref`).toHaveLength(1)
      expect(out.refs[0].ref_type, `${node} mapped to the wrong type`).toBe(refType)
    }
  })

  it('covers the three nodes the editor actually registers', async () => {
    const { REF_TYPES } = await import('../src/editor/extensions/refs')
    expect(Object.keys(REF_TYPES).sort()).toEqual(['driveEmbed', 'issueEmbed', 'mention'])
  })

  // The security invariant again, now against the REAL schema rather than a
  // fixture: whatever a node carries, no label reaches the body.
  it('emits no label for any reference node, whatever attributes it carries', async () => {
    const { REF_TYPES } = await import('../src/editor/extensions/refs')
    const secret = 'Dana Okonkwo — acquisition terms'
    for (const node of Object.keys(REF_TYPES)) {
      const out = extract(
        doc({
          type: node,
          attrs: {
            blockId: 'b',
            refId: '22222222-2222-2222-2222-222222222222',
            // Everything a well-meaning implementation might cache.
            label: secret,
            title: secret,
            name: secret,
            displayName: secret,
          },
        }),
        1,
      )
      expect(JSON.stringify(out), `${node} leaked a label`).not.toContain('Okonkwo')
    }
  })
})

// THE CATCH-UP RULE.
//
// A catch-up projection fires when the stored one is behind the log, and the
// danger it must not create is writing an EMPTY body over a good one — which
// happens if the CRDT has not received its state yet. The server cannot defend
// against that: the seq is the log head either way, so its monotonic guard
// accepts the write and the document silently loses its searchable text.
//
// The rule lived in two places with two different predicates before this. Both
// looked right.
describe('the catch-up publish rule', () => {
  it('refuses a projection with nothing in it', async () => {
    const { worthPublishing } = await import('../src/editor/catchup')
    expect(worthPublishing(extract(doc(), 1))).toBe(false)
    expect(worthPublishing(extract(doc(p('   ', 'a')), 1))).toBe(false)
  })

  it('publishes one with body text', async () => {
    const { worthPublishing } = await import('../src/editor/catchup')
    expect(worthPublishing(extract(doc(p('real content', 'a')), 1))).toBe(true)
  })

  // A document that is nothing but an embed has no body text at all — the
  // extractor deliberately writes no label — but it is NOT empty, and refusing
  // it would leave its backlinks permanently unrepairable.
  it('publishes one that is only a reference', async () => {
    const { worthPublishing } = await import('../src/editor/catchup')
    const out = extract(
      doc({
        type: 'driveEmbed',
        attrs: { blockId: 'e', refId: '55555555-5555-5555-5555-555555555555' },
      }),
      1,
    )
    expect(out.body_text.trim()).toBe('')
    expect(worthPublishing(out)).toBe(true)
  })

  // A heading with no body is a real outline, and a link preview renders from
  // it. Same reasoning as the reference case.
  it('publishes one that is only an outline', async () => {
    const { worthPublishing } = await import('../src/editor/catchup')
    const out = extract(doc(h(1, 'Quarterly plan', 'x')), 1)
    expect(worthPublishing(out)).toBe(true)
  })
})

// THE CATCH-UP CHAIN, END TO END — not one hop short.
//
// The provider test above asserts that a `collab.project` frame reaches
// `onProjectRequest`. It passed while the chain was BROKEN: the screen never
// passed `catchUp` to <Editor>, so the request died one component later, and
// the repair path the server re-asks for every ten minutes did nothing for the
// primary editor. Three independent audits found it; no test did, because every
// test stopped at the provider.
//
// `catchUp` is optional, so tsc could not see the missing prop either. This
// asserts the wiring itself: the source must pass it, and the screen must not
// consume the counter on the block editor's behalf.
describe('the catch-up chain reaches the editor', () => {
  it('passes catchUp from the screen to the Editor', async () => {
    const fs = await import('node:fs/promises')
    const src = await fs.readFile('src/screens/CollabDocumentScreen.tsx', 'utf8')

    // The <Editor> element, from its tag to the self-closing bracket.
    const el = src.match(/<Editor\b[\s\S]*?\/>/)
    expect(el, 'CollabDocumentScreen no longer renders <Editor>').not.toBeNull()
    expect(
      el![0],
      'the Editor is rendered without catchUp, so the server\'s collab.project ' +
        'request and the head_seq/projection_seq gap are both dead for documents',
    ).toContain('catchUp=')
  })

  // The screen's own catch-up serves the sheet and the canvas. It must bail on
  // `document` BEFORE claiming the nonce, or it marks the request answered and
  // publishes nothing — the exact swallow the counter exists to prevent.
  it('does not consume the nonce on the block editor\'s behalf', async () => {
    const fs = await import('node:fs/promises')
    const src = await fs.readFile('src/screens/CollabDocumentScreen.tsx', 'utf8')

    const claim = src.indexOf('caughtUp.current = projectNonce')
    expect(claim, 'the screen no longer claims the nonce').toBeGreaterThan(-1)

    // Walk back to the effect that contains it and check the type guard is
    // inside that effect and above the claim.
    const effectStart = src.lastIndexOf('useEffect(() => {', claim)
    const guard = src.indexOf("fileType !== 'spreadsheet'", effectStart)
    expect(
      guard > -1 && guard < claim,
      'the screen claims the projection nonce before checking the file type, so a ' +
        'document request is marked answered and nothing is published',
    ).toBe(true)
  })
})

// THE NONCE IS CLAIMED WHEN THE WORK IS DONE, NOT WHEN IT IS SCHEDULED.
//
// Both catch-up effects arm a timer and claim the request. Claiming BEFORE the
// timer loses it: the effect's cleanup clears the timer, so any dependency
// change inside the window — a reconnect flips `status` to 'connecting' and
// back — re-runs the effect, finds the nonce claimed, and returns. Request
// answered, nothing published.
//
// Asserted on the source because there is no React renderer in this project,
// and the ordering IS the invariant: the claim must appear after the guard
// inside the callback, not above it.
describe('the catch-up claim', () => {
  it('happens inside the timer, in both surfaces', async () => {
    const fs = await import('node:fs/promises')
    for (const [file, marker] of [
      ['src/screens/CollabDocumentScreen.tsx', 'caughtUp.current = projectNonce'],
      ['src/editor/Editor.web.tsx', 'caughtUp.current = catchUp'],
    ] as const) {
      const whole = await fs.readFile(file, 'utf8')
      // Scope to the catch-up effect. Searching the whole file backwards from
      // the claim finds the DEBOUNCE timer instead, which is a different
      // setTimeout and made the first version of this test pass against the
      // very ordering it exists to pin.
      const start = whole.indexOf('const caughtUp = useRef(0)')
      expect(start, `${file} no longer has the catch-up ref`).toBeGreaterThan(-1)
      const src = whole.slice(start)

      const claim = src.indexOf(marker)
      expect(claim, `${file} no longer claims the nonce`).toBeGreaterThan(-1)
      const timer = src.indexOf('setTimeout(')
      expect(
        timer > -1 && timer < claim,
        `${file} claims the catch-up request before arming its timer, so a ` +
          `dependency change inside the window swallows the request`,
      ).toBe(true)
    }
  })

  // And it happens after the emptiness check, so an empty result leaves the
  // request live for the server's next re-ask rather than burning it.
  it('happens after worthPublishing, in both surfaces', async () => {
    const fs = await import('node:fs/promises')
    for (const [file, marker] of [
      ['src/screens/CollabDocumentScreen.tsx', 'caughtUp.current = projectNonce'],
      ['src/editor/Editor.web.tsx', 'caughtUp.current = catchUp'],
    ] as const) {
      const whole = await fs.readFile(file, 'utf8')
      const src = whole.slice(whole.indexOf('const caughtUp = useRef(0)'))
      const claim = src.indexOf(marker)
      const check = src.indexOf('worthPublishing(projection)')
      expect(
        check > -1 && check < claim,
        `${file} claims the request before checking the projection is worth ` +
          `publishing, so an empty one burns the request`,
      ).toBe(true)
    }
  })
})

describe('projection scheduling waits for durable collaboration state', () => {
  it('flushes on blur without inventing another required server sequence', async () => {
    const fs = await import('node:fs/promises')
    const source = await fs.readFile('src/editor/Editor.web.tsx', 'utf8')

    expect(source).toContain("editor.on('update', schedule)")
    expect(source).toContain("editor.on('blur', flushProjection)")
    expect(source).not.toContain("editor.on('blur', schedule)")
  })

  it('keeps the debounce after an early acknowledgement and flushes at its original floor', async () => {
    vi.useFakeTimers()
    const { ProjectionScheduler } = await import('../src/lib/collab/projectionScheduler')
    const state = { seq: 7, synced: false, pending: true }
    const published: number[] = []
    const scheduler = new ProjectionScheduler({
      delayMs: 2_000,
      readState: () => state,
      build: (seq) => seq,
      publish: (seq) => {
        published.push(seq)
        return true
      },
    })

    scheduler.request()
    state.seq = 8
    state.synced = true
    state.pending = false
    scheduler.notify()
    expect(published, 'acknowledgement bypassed the settle debounce').toEqual([])

    expect(scheduler.flush()).toBe(true)
    expect(published).toEqual([8])
    expect(scheduler.flush(), 'blur created a synthetic requirement for seq 9').toBe(false)
    scheduler.dispose()
  })

  it('flushes every durable surface before destroying its provider', async () => {
    const fs = await import('node:fs/promises')
    const source = await fs.readFile('src/screens/CollabDocumentScreen.tsx', 'utf8')
    const cleanupStart = source.indexOf('return () => {', source.indexOf('const provider = new CollabProvider'))
    const cleanupEnd = source.indexOf('}, [documentId', cleanupStart)
    const cleanup = source.slice(cleanupStart, cleanupEnd)
    const surfaceFlush = cleanup.indexOf('surfaceSchedulerRef.current?.flush()')
    const documentFlush = cleanup.indexOf('documentProjectionFlushRef.current()')
    const destroy = cleanup.indexOf('provider.destroy()')

    expect(surfaceFlush).toBeGreaterThan(-1)
    expect(documentFlush).toBeGreaterThan(-1)
    expect(surfaceFlush).toBeLessThan(destroy)
    expect(documentFlush).toBeLessThan(destroy)
  })

  it('renders a replacement provider boundary as connecting until that provider reports status', async () => {
    const fs = await import('node:fs/promises')
    const source = await fs.readFile('src/screens/CollabDocumentScreen.tsx', 'utf8')

    expect(source).toContain('const providerBoundary =')
    expect(source).toContain("providerLease.boundary === providerBoundary ? providerLease.status : 'connecting'")
    expect(source).toContain('boundary: providerBoundary, status: s')
  })

  it('keeps cleanup bound to the document publish target that created the scheduler', async () => {
    const fs = await import('node:fs/promises')
    const source = await fs.readFile('src/editor/Editor.web.tsx', 'utf8')

    expect(
      source,
      'a route change can make an old editor cleanup publish its content to the new file',
    ).not.toContain('onProjectRef')
    expect(source).toContain('publish: onProject')
    expect(source).toContain('}, [editor, onProject, registerProjectionFlush])')
  })

  it('keeps a pending editor projection alive across status and profile renders', async () => {
    const fs = await import('node:fs/promises')
    const editorSource = await fs.readFile('src/editor/Editor.web.tsx', 'utf8')
    const screenSource = await fs.readFile('src/screens/CollabDocumentScreen.tsx', 'utf8')

    // A saving -> synced render and a profile refresh must update the existing
    // editor. Recreating it disposes the scheduler while its dirty floor is
    // still unsafe, so the later acknowledgement has nothing left to publish.
    expect(screenSource).toContain('user={identity}')
    expect(editorSource).toContain('[doc, awareness, resolver]')
    expect(editorSource).not.toContain('[doc, awareness, user, resolver]')
    expect(editorSource).toContain('editor?.commands.updateUser(user)')
    expect(editorSource).toContain('[editor, user.id, user.name, user.color]')
    expect(editorSource).toContain('[extensions],')
    expect(editorSource).not.toContain('[extensions, editable]')
  })

  it('retains a document edit until its acknowledged sequence is projectable', async () => {
    vi.useFakeTimers()
    const { ProjectionScheduler } = await import('../src/lib/collab/projectionScheduler')
    const state = { seq: 7, synced: false, pending: true }
    let body = 'draft'
    const published: Array<{ seq: number; body: string }> = []
    const scheduler = new ProjectionScheduler({
      delayMs: 2_000,
      readState: () => state,
      build: (seq) => ({ seq, body }),
      publish: (projection) => {
        published.push(projection)
        return true
      },
    })

    scheduler.request()
    body = 'latest durable content'
    await vi.advanceTimersByTimeAsync(2_000)
    expect(published).toEqual([])

    // Merely becoming "synced" at the edit's old watermark is not enough.
    state.synced = true
    state.pending = false
    scheduler.notify()
    expect(published).toEqual([])

    state.seq = 8
    scheduler.notify()
    expect(published).toEqual([{ seq: 8, body: 'latest durable content' }])
    scheduler.dispose()
  })

  it('retains sheet and design work across an unsafe timer and regenerates it at the new sequence', async () => {
    vi.useFakeTimers()
    const { ProjectionScheduler } = await import('../src/lib/collab/projectionScheduler')

    for (const surface of ['spreadsheet', 'design']) {
      const state = { seq: 12, synced: false, pending: true }
      let revision = 1
      const published: Array<{ seq: number; surface: string; revision: number }> = []
      const scheduler = new ProjectionScheduler({
        delayMs: 2_000,
        readState: () => state,
        build: (seq) => ({ seq, surface, revision }),
        publish: (projection) => {
          published.push(projection)
          return true
        },
      })

      scheduler.request()
      revision = 2
      await vi.advanceTimersByTimeAsync(2_000)
      expect(published, surface).toEqual([])
      expect(scheduler.flush(), `${surface} unsafe unmount flush`).toBe(false)

      state.seq = 13
      state.synced = true
      state.pending = false
      scheduler.notify()
      expect(published, surface).toEqual([{ seq: 13, surface, revision: 2 }])
      scheduler.dispose()
    }
  })
})
