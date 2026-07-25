import { describe, it, expect } from 'vitest'
import { renderMentions, MENTION_PATTERN } from '../src/api/comments'
import { categoryColor, PRIORITIES } from '../src/api/issues'

const A = '7c9e6679-7425-40de-944b-e07fc1f90ae7'
const B = '3f2504e0-4f89-41d3-9a0c-0305e82c3301'

describe('renderMentions', () => {
  const nameFor = (id: string) => (id === A ? 'Alice' : id === B ? 'Bob' : 'someone')

  it('splits a body into text and mentions', () => {
    const segs = renderMentions(`hey <@${A}> look at this`, nameFor)
    expect(segs).toEqual([
      { text: 'hey ', mention: false },
      { text: '@Alice', mention: true },
      { text: ' look at this', mention: false },
    ])
  })

  it('handles a mention at each end and two in a row', () => {
    const segs = renderMentions(`<@${A}><@${B}>`, nameFor)
    expect(segs.map((s) => s.text)).toEqual(['@Alice', '@Bob'])
    expect(segs.every((s) => s.mention)).toBe(true)
  })

  it('leaves a bare @name alone', () => {
    // Names are not unique and they change. Resolving one at render time would
    // make a comment mean something different tomorrow — so the composer encodes
    // a picked person and a typed "@alice" is just text.
    const segs = renderMentions('hey @alice', nameFor)
    expect(segs).toEqual([{ text: 'hey @alice', mention: false }])
  })

  it('renders an unknown id as a placeholder rather than a uuid', () => {
    const segs = renderMentions(`<@${A}>`, () => 'someone')
    expect(segs[0].text).toBe('@someone')
  })

  it('does not lose text when there are no mentions', () => {
    expect(renderMentions('plain text', nameFor)).toEqual([{ text: 'plain text', mention: false }])
    expect(renderMentions('', nameFor)).toEqual([])
  })

  it('uses a fresh matcher each call', () => {
    // MENTION_PATTERN is a /g regex, which carries lastIndex. Reusing it across
    // calls without resetting would make every second render drop its first
    // mention — a bug that only appears on the second comment in a thread.
    const body = `<@${A}>`
    expect(renderMentions(body, nameFor)).toHaveLength(1)
    expect(renderMentions(body, nameFor)).toHaveLength(1)
    expect(MENTION_PATTERN.lastIndex).toBe(0)
  })
})

describe('issue vocabulary', () => {
  it('gives every state category a colour', () => {
    // The CATEGORY decides the colour, not the state's name — names are
    // per-project and mean different things to two teams.
    for (const c of ['backlog', 'unstarted', 'started', 'completed', 'cancelled'] as const) {
      expect(categoryColor(c)).toMatch(/^#[0-9a-f]{6}$/i)
    }
  })

  it('orders priorities so a sort is a sort, with 0 meaning none', () => {
    expect(PRIORITIES[0]).toEqual({ value: 0, label: 'No priority' })
    const values = PRIORITIES.map((p) => p.value)
    expect(values).toEqual([...values].sort((a, b) => a - b))
    // Urgent must sort ahead of Low, which is what makes ORDER BY priority
    // meaningful on the server.
    const urgent = PRIORITIES.find((p) => p.label === 'Urgent')!
    const low = PRIORITIES.find((p) => p.label === 'Low')!
    expect(urgent.value).toBeLessThan(low.value)
  })
})
