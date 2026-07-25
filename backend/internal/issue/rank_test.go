package issue

import (
	"errors"
	"math/rand"
	"sort"
	"strings"
	"testing"
)

func TestBetweenProducesSomethingStrictlyInside(t *testing.T) {
	tests := []struct{ lo, hi string }{
		{"", ""},
		{"", "U"},
		{"U", ""},
		{"A", "B"}, // adjacent digits — the case that forces a longer string
		{"A", "z"},
		{"0", "1"}, // adjacent at the bottom
		{"y", "z"}, // adjacent at the top
		{"AAAA", "AAAB"},
		{"U", "V"},
	}
	for _, tt := range tests {
		got, err := Between(tt.lo, tt.hi)
		if err != nil {
			t.Fatalf("Between(%q,%q): %v", tt.lo, tt.hi, err)
		}
		if tt.lo != "" && got <= tt.lo {
			t.Errorf("Between(%q,%q) = %q, which does not sort after the lower bound", tt.lo, tt.hi, got)
		}
		if tt.hi != "" && got >= tt.hi {
			t.Errorf("Between(%q,%q) = %q, which does not sort before the upper bound", tt.lo, tt.hi, got)
		}
	}
}

// The first item is in the MIDDLE of the space, because the first thing anybody
// does to a one-item list is drag something above it.
func TestInitialLeavesRoomAbove(t *testing.T) {
	above, err := Between("", Initial)
	if err != nil {
		t.Fatalf("nothing fits above the first item: %v", err)
	}
	if above >= Initial {
		t.Errorf("Between(\"\", %q) = %q, which is not above it", Initial, above)
	}
	below, err := Between(Initial, "")
	if err != nil {
		t.Fatalf("nothing fits below the first item: %v", err)
	}
	if below <= Initial {
		t.Errorf("Between(%q, \"\") = %q, which is not below it", Initial, below)
	}
}

// THE PROPERTY THAT KILLS A FLOAT REPRESENTATION.
//
// Halving between two neighbours runs out of mantissa after ~50 inserts in the
// same gap — silently. Two cards then compare equal and the board flickers
// between two orders on every read, which is a bug report that says "sometimes
// it's wrong".
//
// Here the same operation grows the string instead, so it keeps working. This
// drives it far past where a float dies.
func TestRepeatedInsertionIntoOneGapKeepsWorking(t *testing.T) {
	lo, hi := "A", "B" // adjacent digits: the tightest possible starting gap

	current := hi
	for i := range 200 {
		next, err := Between(lo, current)
		if err != nil {
			t.Fatalf("insert %d into the same gap failed: %v (a float dies around 50)", i, err)
		}
		if next <= lo || next >= current {
			t.Fatalf("insert %d produced %q, which is not inside (%q, %q)", i, next, lo, current)
		}
		current = next
	}
	if len(current) > MaxRankLength {
		t.Fatalf("rank grew past the limit without reporting it: %d chars", len(current))
	}
}

// A randomised board: insert at random positions, and after every insert the
// stored order must equal the intended order.
//
// This is the test that would catch an off-by-one in `between`'s walk, which no
// hand-written case does — the failure needs a specific pair of neighbours that
// nobody thinks to write down.
func TestRandomInsertionsKeepTheOrder(t *testing.T) {
	rng := rand.New(rand.NewSource(1)) // fixed seed: a flake here must be reproducible

	type card struct {
		rank string
		seq  int // the intended order, independent of the rank
	}
	cards := []card{}

	for i := range 400 {
		pos := 0
		if len(cards) > 0 {
			pos = rng.Intn(len(cards) + 1)
		}
		lo, hi := "", ""
		if pos > 0 {
			lo = cards[pos-1].rank
		}
		if pos < len(cards) {
			hi = cards[pos].rank
		}

		rank, err := Between(lo, hi)
		if err != nil {
			t.Fatalf("insert %d at position %d of %d: %v", i, pos, len(cards), err)
		}

		cards = append(cards, card{})
		copy(cards[pos+1:], cards[pos:])
		cards[pos] = card{rank: rank, seq: i}

		// The ranks, sorted BYTEWISE, must be the order the cards are actually in.
		// Bytewise because that is what COLLATE "C" does in Postgres, and a test
		// that used any other comparison would be testing a different database.
		ranks := make([]string, len(cards))
		for j, c := range cards {
			ranks[j] = c.rank
		}
		if !sort.SliceIsSorted(ranks, func(a, b int) bool { return ranks[a] < ranks[b] }) {
			t.Fatalf("after insert %d the ranks are out of order: %v", i, ranks)
		}
		for j := 1; j < len(ranks); j++ {
			if ranks[j-1] == ranks[j] {
				t.Fatalf("after insert %d two cards share the rank %q", i, ranks[j])
			}
		}
	}
}

// Every rank must match the column's CHECK constraint, or the insert fails at
// the database with a message nobody can act on.
func TestEveryGeneratedRankMatchesTheColumnConstraint(t *testing.T) {
	const allowed = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

	check := func(rank string) {
		t.Helper()
		if rank == "" {
			t.Fatal("an empty rank would fail the NOT NULL / shape constraint")
		}
		for i := range len(rank) {
			if !strings.ContainsRune(allowed, rune(rank[i])) {
				t.Fatalf("rank %q contains %q, which issues_rank_shape rejects", rank, rank[i])
			}
		}
	}

	check(Initial)
	lo := ""
	for range 100 {
		r, err := Between(lo, "")
		if err != nil {
			t.Fatal(err)
		}
		check(r)
		lo = r
	}
	ranks, err := Renormalise(500)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range ranks {
		check(r)
	}
}

func TestRenormaliseProducesAnOrderedSpreadWithRoomBetween(t *testing.T) {
	for _, n := range []int{1, 2, 10, 500, 5000} {
		ranks, err := Renormalise(n)
		if err != nil {
			t.Fatalf("Renormalise(%d): %v", n, err)
		}
		if len(ranks) != n {
			t.Fatalf("Renormalise(%d) returned %d ranks", n, len(ranks))
		}
		for i := 1; i < len(ranks); i++ {
			if ranks[i-1] >= ranks[i] {
				t.Fatalf("Renormalise(%d) is not ordered at %d: %q >= %q", n, i, ranks[i-1], ranks[i])
			}
			// Room to insert between every adjacent pair — the point of
			// renormalising is that the NEXT insert does not immediately grow.
			if _, err := Between(ranks[i-1], ranks[i]); err != nil {
				t.Fatalf("Renormalise(%d) left no room between %q and %q: %v",
					n, ranks[i-1], ranks[i], err)
			}
		}
	}
}

// A caller that hands the bounds over in the wrong order has a bug: the server
// derives both from a requested position, so this is never user input.
func TestBetweenRefusesInvertedBounds(t *testing.T) {
	if _, err := Between("z", "a"); !errors.Is(err, ErrRankOrder) {
		t.Errorf("Between(\"z\",\"a\") = %v, want ErrRankOrder", err)
	}
	if _, err := Between("A", "A"); !errors.Is(err, ErrRankOrder) {
		t.Errorf("Between of two equal ranks = %v, want ErrRankOrder", err)
	}
}

func TestBetweenRejectsRanksTheColumnCouldNotHold(t *testing.T) {
	if _, err := Between("not a rank!", ""); err == nil {
		t.Error("a rank outside the alphabet was accepted; it would sort unpredictably")
	}
	if _, err := Between(strings.Repeat("A", MaxRankLength+1), ""); err == nil {
		t.Error("an over-long rank was accepted")
	}
}
