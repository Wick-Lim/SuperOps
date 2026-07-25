package issue

import (
	"errors"
	"fmt"
	"strings"
)

// Ordering.
//
// docs/plans/03-work-tracking.md §5 calls this the hard part, and the reason is
// that every obvious representation is wrong in a way that only shows up later:
//
//	an integer position   — reordering one card rewrites every row after it, so
//	                        a drag is an UPDATE over the whole column and two
//	                        drags at once deadlock.
//	a float               — halving between neighbours runs out of mantissa after
//	                        ~50 inserts in the same gap, silently, and then two
//	                        cards compare equal and the board flickers between
//	                        two orders on every read.
//	a linked list         — reading a column in order is N round trips.
//
// So: a variable-length string over a fixed alphabet, compared BYTEWISE, with a
// "give me something between these two" operation that can always succeed by
// growing. That is what the column's `TEXT COLLATE "C"` is for — C collation is
// byte order, and under any other collation `'a' < 'B'` is a question about
// language rather than about bytes, so the same ranks would sort differently on
// a server with a different locale.

// alphabet is the rank digit set, in byte order. Digits before uppercase before
// lowercase, which is ASCII order, which is what COLLATE "C" compares.
//
// 62 symbols rather than the full byte range: a rank ends up in URLs, logs and
// cursors, and a non-printable byte in any of those is a debugging session.
const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// MaxRankLength bounds a rank.
//
// Growth is one character per insert into the SAME gap, so this permits 64
// consecutive "drop it right here" operations between two fixed neighbours
// before Between refuses. In practice a gap widens as soon as anything else is
// inserted anywhere near it; the limit exists so a pathological loop reports a
// problem instead of writing a megabyte into a column.
const MaxRankLength = 64

// ErrRankExhausted means two neighbours are too close to fit anything between
// them at the length limit. The caller's remedy is to renormalise the column,
// not to retry.
var ErrRankExhausted = errors.New("issue: no rank fits between these two; the column needs renormalising")

// ErrRankOrder means the caller passed neighbours in the wrong order, which is a
// bug rather than a user error: the server derives both from the requested
// position, so getting here means the position was resolved incorrectly.
var ErrRankOrder = errors.New("issue: the lower rank must sort before the upper one")

// Initial is the rank of the first item in an empty column.
//
// Deliberately in the MIDDLE of the space rather than at the start: the first
// thing anybody does to a one-item list is drag something above it, and a first
// rank of "0" would leave nowhere above to go.
const Initial = "U"

// Between returns a rank that sorts strictly after lo and strictly before hi.
//
// Either bound may be empty: an empty lo means "the beginning", an empty hi
// means "the end". That is what makes the caller's job "name the neighbours",
// which it can always do from a position, rather than "compute a rank", which it
// cannot.
func Between(lo, hi string) (string, error) {
	if err := validate(lo); err != nil {
		return "", fmt.Errorf("lower bound: %w", err)
	}
	if err := validate(hi); err != nil {
		return "", fmt.Errorf("upper bound: %w", err)
	}
	if lo != "" && hi != "" && lo >= hi {
		return "", fmt.Errorf("%w: %q >= %q", ErrRankOrder, lo, hi)
	}

	switch {
	case lo == "" && hi == "":
		return Initial, nil
	case lo == "":
		return before(hi)
	case hi == "":
		return after(lo)
	default:
		return between(lo, hi)
	}
}

// between finds a string strictly inside (lo, hi).
//
// It walks the two bounds a character at a time. While they agree, the answer
// agrees too. At the first disagreement, if there is room between the two
// digits, the answer takes the midpoint and stops. If there is not — they are
// adjacent digits — the answer keeps lo's prefix and recurses on the remainder,
// which is what makes it always terminate with a LONGER string rather than
// failing.
func between(lo, hi string) (string, error) {
	var out strings.Builder

	for i := 0; ; i++ {
		l := digitAt(lo, i, 0)             // past the end of lo, treat as the lowest
		h := digitAt(hi, i, len(alphabet)) // past the end of hi, treat as one past the highest

		if l == h {
			if out.Len() >= MaxRankLength {
				return "", ErrRankExhausted
			}
			out.WriteByte(alphabet[l])
			continue
		}
		if h-l > 1 {
			// Room between them: take the midpoint and stop.
			out.WriteByte(alphabet[(l+h)/2])
			return out.String(), nil
		}

		// Adjacent digits. Keep the lower one and continue past the end of lo,
		// where hi no longer constrains anything — so the rest of the problem is
		// "something after lo's tail", which after() answers.
		out.WriteByte(alphabet[l])
		tail, err := after(sliceFrom(lo, i+1))
		if err != nil {
			return "", err
		}
		if out.Len()+len(tail) > MaxRankLength {
			return "", ErrRankExhausted
		}
		out.WriteString(tail)
		return out.String(), nil
	}
}

// after returns a rank strictly greater than lo.
//
// It bumps the last digit when it can, and appends a middling digit when it
// cannot — appending ALWAYS produces something greater, because a string is
// greater than its own prefix.
func after(lo string) (string, error) {
	if lo == "" {
		return Initial, nil
	}
	last := index(lo[len(lo)-1])
	if last+1 < len(alphabet)-1 {
		// Step to the midpoint of what is left above, not to the very next
		// digit: stepping by one wastes the whole remaining space on one insert
		// and the next append is immediately longer.
		return lo[:len(lo)-1] + string(alphabet[(last+len(alphabet))/2]), nil
	}
	if len(lo)+1 > MaxRankLength {
		return "", ErrRankExhausted
	}
	return lo + Initial, nil
}

// before returns a rank strictly less than hi.
func before(hi string) (string, error) {
	if hi == "" {
		return Initial, nil
	}
	first := index(hi[len(hi)-1])
	if first > 1 {
		return hi[:len(hi)-1] + string(alphabet[first/2]), nil
	}
	// hi ends in the lowest digit, so nothing shorter fits under it. Keep it and
	// go one level deeper: "<hi minus its last digit><lowest><middle>" sorts
	// below hi and above everything shorter.
	if len(hi)+1 > MaxRankLength {
		return "", ErrRankExhausted
	}
	return hi[:len(hi)-1] + string(alphabet[0]) + Initial, nil
}

// Renormalise spreads n items evenly across the space, returning the ranks in
// order.
//
// It is the answer to ErrRankExhausted and it is deliberately a whole-column
// rewrite: a partial one leaves the rewritten items adjacent to unrewritten ones
// with no guarantee about the gap, which is the same problem one insert later.
func Renormalise(n int) ([]string, error) {
	if n <= 0 {
		return nil, nil
	}
	if n == 1 {
		return []string{Initial}, nil
	}

	// Choose the shortest width that leaves room: n items need n distinct
	// values, and stepping by more than one digit keeps room to insert between
	// them afterwards.
	width, capacity := 1, len(alphabet)
	for capacity < n*2 {
		width++
		capacity *= len(alphabet)
		if width > 8 {
			return nil, fmt.Errorf("issue: %d items is past what a rank can address", n)
		}
	}

	step := capacity / (n + 1)
	if step < 1 {
		step = 1
	}
	out := make([]string, n)
	for i := range n {
		out[i] = encode((i+1)*step, width)
	}
	return out, nil
}

// encode renders a base-62 value at a fixed width, most significant first.
func encode(value, width int) string {
	buf := make([]byte, width)
	for i := width - 1; i >= 0; i-- {
		buf[i] = alphabet[value%len(alphabet)]
		value /= len(alphabet)
	}
	return string(buf)
}

func validate(rank string) error {
	if rank == "" {
		return nil
	}
	if len(rank) > MaxRankLength {
		return fmt.Errorf("a rank may be at most %d characters", MaxRankLength)
	}
	for i := range len(rank) {
		if index(rank[i]) < 0 {
			return fmt.Errorf("%q is not a rank digit", rank[i])
		}
	}
	return nil
}

func index(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'A' && c <= 'Z':
		return int(c-'A') + 10
	case c >= 'a' && c <= 'z':
		return int(c-'a') + 36
	default:
		return -1
	}
}

// digitAt returns the alphabet index of rank[i], or def when i is past the end.
func digitAt(rank string, i, def int) int {
	if i >= len(rank) {
		return def
	}
	return index(rank[i])
}

func sliceFrom(s string, i int) string {
	if i >= len(s) {
		return ""
	}
	return s[i:]
}
