import { describe, expect, it } from 'vitest'
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
