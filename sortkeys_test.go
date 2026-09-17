package main

import (
	"errors"
	"fmt"
	"math"
	"math/rand"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/tsdb/chunks"
)

// lsetReader serves label sets by ref, nothing else.
type lsetReader struct {
	tsdb.IndexReader
	lsets []labels.Labels
}

func (r lsetReader) Series(ref storage.SeriesRef, b *labels.ScratchBuilder, _ *[]chunks.Meta) error {
	b.Reset()
	for _, l := range r.lsets[ref] {
		b.Add(l.Name, l.Value)
	}
	return nil
}

// TestSortTenant sorts random tenants the way streamSplitBlock does and checks the
// result against slices.SortFunc(labels.Compare). Names and values are short
// strings that are prefixes of each other, and label sets are prefixes of each
// other, so symbol index order and label set length both matter.
func TestSortTenant(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	strs := []string{"a", "a_", "ab", "b", "b0", "ba"}
	randomLset := func() labels.Labels {
		m := map[string]string{}
		for n := rng.Intn(4); len(m) < n; {
			m[strs[rng.Intn(len(strs))]] = strs[rng.Intn(len(strs))]
		}
		return labels.FromMap(m)
	}
	sorted := 0
	for tc := 0; tc < 3000; tc++ {
		lsets := make([]labels.Labels, 2+rng.Intn(100))
		for i := range lsets {
			lsets[i] = randomLset()
		}
		slices.SortFunc(lsets, labels.Compare)
		lsets = slices.CompactFunc(lsets, labels.Equal)
		switch tc % 3 {
		case 0: // shuffle
			rng.Shuffle(len(lsets), func(i, j int) { lsets[i], lsets[j] = lsets[j], lsets[i] })
		case 1: // move one series
			i, j := rng.Intn(len(lsets)), rng.Intn(len(lsets))
			l := lsets[i]
			lsets = slices.Insert(slices.Delete(lsets, i, i+1), j, l)
		}
		if tc%4 == 0 && len(lsets) > 2 { // duplicate one series
			lsets[rng.Intn(len(lsets))] = lsets[rng.Intn(len(lsets))]
		}

		// Discovery.
		var d disorder
		syms := map[string]uint32{}
		refs := make([]storage.SeriesRef, len(lsets))
		var counts labelCounts
		adjacentDup := false
		for i, l := range lsets {
			refs[i] = storage.SeriesRef(i)
			counts.add(len(l))
			for _, x := range l {
				syms[x.Name], syms[x.Value] = 0, 0
			}
			if i > 0 {
				switch c := labels.Compare(l, lsets[i-1]); {
				case c == 0:
					adjacentDup = true
				case c < 0:
					d.add(lsets[i-1], l)
				}
			}
		}
		if adjacentDup || d.minStart == nil {
			continue
		}
		symbols := make([]string, 0, len(syms))
		for s := range syms {
			symbols = append(symbols, s)
		}
		slices.Sort(symbols)
		for i, s := range symbols {
			syms[s] = uint32(i)
		}

		want := slices.Clone(lsets)
		slices.SortFunc(want, labels.Compare)
		wantDup := len(slices.CompactFunc(slices.Clone(want), labels.Equal)) != len(want)
		keep := func(l labels.Labels) (labels.Labels, bool) { return l, true }
		_, _, err := sortTenant(lsetReader{lsets: lsets}, refs, d, keep, symbols, syms, counts)
		if wantDup {
			if !errors.Is(err, errStreamDuplicateSeries) {
				t.Fatalf("case %d: err = %v, want errStreamDuplicateSeries for %v", tc, err, lsets)
			}
			continue
		}
		if err != nil {
			t.Fatalf("case %d: %v", tc, err)
		}
		for i, ref := range refs {
			if !labels.Equal(lsets[ref], want[i]) {
				t.Fatalf("case %d: position %d is %s, want %s", tc, i, lsets[ref], want[i])
			}
		}
		sorted++
	}
	if sorted < 1000 {
		t.Errorf("only %d cases sorted", sorted)
	}
}

// TestSortTenantRange checks that one series displaced by 66 in 340000 only sorts
// the series it moved across, like source block A.
func TestSortTenantRange(t *testing.T) {
	const n, from, to = 340000, 339493, 339559
	lsets := make([]labels.Labels, 0, n)
	for i := 0; i < n; i++ {
		lsets = append(lsets, labels.FromStrings("k", fmt.Sprintf("%06d", i)))
	}
	moved := lsets[to]
	lsets = slices.Insert(slices.Delete(lsets, to, to+1), from, moved)
	var d disorder
	var counts labelCounts
	refs := make([]storage.SeriesRef, n)
	for i := range refs {
		refs[i] = storage.SeriesRef(i)
		counts.add(len(lsets[i]))
		if i > 0 && labels.Compare(lsets[i], lsets[i-1]) < 0 {
			d.add(lsets[i-1], lsets[i])
		}
	}
	symbols := []string{"k"}
	syms := map[string]uint32{}
	for _, l := range lsets {
		symbols = append(symbols, l.Get("k"))
	}
	slices.Sort(symbols)
	for i, s := range symbols {
		syms[s] = uint32(i)
	}
	keep := func(l labels.Labels) (labels.Labels, bool) { return l, true }
	lo, hi, err := sortTenant(lsetReader{lsets: lsets}, refs, d, keep, symbols, syms, counts)
	if err != nil {
		t.Fatal(err)
	}
	if lo != from || hi != to+1 {
		t.Errorf("sorted [%d, %d), want [%d, %d)", lo, hi, from, to+1)
	}
	for i := 1; i < n; i++ {
		if labels.Compare(lsets[refs[i]], lsets[refs[i-1]]) <= 0 {
			t.Fatalf("position %d is out of order", i)
		}
	}
}

// TestLabelCountsBound checks that bound(n) is never below the labels of any n
// consecutive series, so sortTenant's arena never grows, and that it is exact for
// the whole tenant.
func TestLabelCountsBound(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for tc := 0; tc < 300; tc++ {
		sizes := make([]int, 1+rng.Intn(60))
		var c labelCounts
		for i := range sizes {
			sizes[i] = rng.Intn(1 + rng.Intn(20))
			c.add(sizes[i])
		}
		if got := c.bound(len(sizes)); got != c.total {
			t.Fatalf("case %d: bound(all) = %d, want total %d", tc, got, c.total)
		}
		for lo := range sizes {
			sum := 0
			for hi := lo + 1; hi <= len(sizes); hi++ {
				sum += sizes[hi-1]
				if b := c.bound(hi - lo); b < sum {
					t.Fatalf("case %d: bound(%d) = %d < %d labels in sizes[%d:%d] of %v", tc, hi-lo, b, sum, lo, hi, sizes)
				}
			}
		}
	}
}

// reversedFamily returns n series of one metric whose __service label orders them
// opposite to pod, so each series starts a new run once __service is stripped.
func reversedFamily(n int) []floatSeries {
	s := make([]floatSeries, 0, n)
	for i := 0; i < n; i++ {
		s = append(s, floatSeries{labels.FromStrings("__name__", "m", "__service", fmt.Sprintf("%03d", n-1-i), "pod", fmt.Sprintf("%03d", i), "prometheus", "A"), 0, 3})
	}
	return s
}

// extLastChains returns {a=1}, {a=1, b=2}, {a=1, b=2, c=3} with the ext label
// prometheus last, for metrics metric names. Source order puts the longest first.
func extLastChains(metrics int) []floatSeries {
	var s []floatSeries
	for i := 0; i < metrics; i++ {
		name := fmt.Sprintf("m%02d", i)
		s = append(s,
			floatSeries{labels.FromStrings("__name__", name, "a", "1", "prometheus", "A"), 0, 3},
			floatSeries{labels.FromStrings("__name__", name, "a", "1", "b", "2", "prometheus", "A"), 0, 3},
			floatSeries{labels.FromStrings("__name__", name, "a", "1", "b", "2", "c", "3", "prometheus", "A"), 0, 3},
		)
	}
	return s
}

// TestLabelCountsBoundTight: with two series sizes, bound(n) is exactly the labels
// of the n largest series, so a looser bound fails.
func TestLabelCountsBoundTight(t *testing.T) {
	var c labelCounts
	for _, n := range []int{5, 1, 5, 1} {
		c.add(n)
	}
	for n, want := range []int{0, 5, 10, 11, 12} {
		if got := c.bound(n); got != want {
			t.Errorf("bound(%d) = %d, want %d (sizes 5, 1, 5, 1)", n, got, want)
		}
	}
}

type failingReader struct {
	tsdb.IndexReader
	err error
}

func (r failingReader) Series(storage.SeriesRef, *labels.ScratchBuilder, *[]chunks.Meta) error {
	return r.err
}

// TestSortTenantReadError: an index read error keeps its cause and is not taken
// for a duplicate, which would send the block to the Head fallback.
func TestSortTenantReadError(t *testing.T) {
	a, b := labels.FromStrings("k", "a"), labels.FromStrings("k", "b")
	var c labelCounts
	c.add(1)
	c.add(1)
	boom := errors.New("boom")
	keep := func(l labels.Labels) (labels.Labels, bool) { return l, true }
	_, _, err := sortTenant(failingReader{err: boom}, []storage.SeriesRef{0, 1}, disorder{minStart: a, maxEnd: b}, keep,
		[]string{"a", "b", "k"}, map[string]uint32{"a": 0, "b": 1, "k": 2}, c)
	if !errors.Is(err, boom) || errors.Is(err, errStreamDuplicateSeries) {
		t.Errorf("err = %v, want the read error and not errStreamDuplicateSeries", err)
	}
}

// TestSortTenantGuards covers the empty-range and uint32-overflow errors.
func TestSortTenantGuards(t *testing.T) {
	f := labels.FromStrings
	src := []labels.Labels{f("k", "a"), f("k", "c"), f("k", "b"), f("k", "d")}
	d := disorder{minStart: src[2], maxEnd: src[1]}
	symbols := []string{"a", "b", "c", "d", "k"}
	syms := map[string]uint32{"a": 0, "b": 1, "c": 2, "d": 3, "k": 4}
	var c labelCounts
	for range src {
		c.add(1)
	}
	keep := func(l labels.Labels) (labels.Labels, bool) { return l, true }
	// a re-read that disagrees with discovery: every series is now greater than maxEnd
	later := lsetReader{lsets: []labels.Labels{f("k", "x"), f("k", "y"), f("k", "y0"), f("k", "z")}}
	if _, _, err := sortTenant(later, []storage.SeriesRef{0, 1, 2, 3}, d, keep, symbols, syms, c); err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Errorf("empty range: err = %v", err)
	}
	big := labelCounts{series: 4, total: math.MaxUint32/2 + 1, min: 1, max: 1}
	if _, _, err := sortTenant(lsetReader{lsets: src}, []storage.SeriesRef{0, 1, 2, 3}, d, keep, symbols, syms, big); err == nil || !strings.Contains(err.Error(), "too many") {
		t.Errorf("uint32 overflow: err = %v", err)
	}
}

// TestSortTenantArenaAlloc checks that the sort allocates its key arena once, sized
// for the sorted range and not for the tenant, so it neither grows nor scales with
// tenant size.
func TestSortTenantArenaAlloc(t *testing.T) {
	const from, size, wide = 10000, 1000, 20 // series [from, from+size) carry wide extra labels
	build := func(n int) (lsets []labels.Labels, refs []storage.SeriesRef, d disorder, symbols []string, syms map[string]uint32, c labelCounts, rangeLabels int) {
		syms = map[string]uint32{}
		for i := 0; i < n; i++ {
			l := []string{"k", fmt.Sprintf("%07d", i)}
			if i >= from && i < from+size {
				for j := 0; j < wide; j++ {
					l = append(l, fmt.Sprintf("l%02d", j), "v")
				}
			}
			lsets = append(lsets, labels.FromStrings(l...))
		}
		moved := lsets[from] // behind the other wide series, so [from, from+size) is sorted
		lsets = slices.Insert(slices.Delete(lsets, from, from+1), from+size-1, moved)
		refs = make([]storage.SeriesRef, n)
		for i, l := range lsets {
			refs[i] = storage.SeriesRef(i)
			c.add(len(l))
			if i > 0 && labels.Compare(l, lsets[i-1]) < 0 {
				d.add(lsets[i-1], l)
			}
			for _, x := range l {
				syms[x.Name], syms[x.Value] = 0, 0
			}
		}
		for s := range syms {
			symbols = append(symbols, s)
		}
		slices.Sort(symbols)
		for i, s := range symbols {
			syms[s] = uint32(i)
		}
		return lsets, refs, d, symbols, syms, c, size * (1 + wide)
	}
	keep := func(l labels.Labels) (labels.Labels, bool) { return l, true }
	measure := func(n int) (sortAlloc, readAlloc uint64, rangeLabels int) {
		lsets, refs, d, symbols, syms, c, rl := build(n)
		ir := lsetReader{lsets: lsets}
		var ms runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&ms)
		before := ms.TotalAlloc
		lo, hi, err := sortTenant(ir, refs, d, keep, symbols, syms, c)
		runtime.ReadMemStats(&ms)
		sortAlloc = ms.TotalAlloc - before
		if err != nil || lo != from || hi != from+size {
			t.Fatalf("n=%d: sorted [%d, %d) err %v, want [%d, %d)", n, lo, hi, err, from, from+size)
		}
		// the same reads without the sort: the range plus both binary searches
		var b labels.ScratchBuilder
		var chks []chunks.Meta
		runtime.GC()
		runtime.ReadMemStats(&ms)
		before = ms.TotalAlloc
		for i := lo; i < hi; i++ {
			_ = ir.Series(refs[i], &b, &chks)
			keep(b.Labels())
		}
		for s := 0; s < 2; s++ {
			for lo, hi := 0, n; lo < hi; {
				m := int(uint(lo+hi) >> 1)
				_ = ir.Series(refs[m], &b, &chks)
				keep(b.Labels())
				lo = m + 1
			}
		}
		runtime.ReadMemStats(&ms)
		return sortAlloc, ms.TotalAlloc - before, rl
	}
	for _, n := range []int{20000, 200000} {
		sortAlloc, readAlloc, rangeLabels := measure(n)
		arena := uint64(8*rangeLabels + 16*size)
		t.Logf("n=%d: sortTenant allocated %d B, reads %d B, arena+keys %d B, rest %d B", n, sortAlloc, readAlloc, arena, int64(sortAlloc)-int64(readAlloc)-int64(arena))
		if sortAlloc > readAlloc+arena+arena/4 {
			t.Errorf("n=%d: sortTenant allocated %d B, want at most %d B of reads plus %d B of arena and keys (+25%%)", n, sortAlloc, readAlloc, arena)
		}
	}
}
