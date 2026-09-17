package main

import (
	"fmt"
	"math"
	"slices"
	"sort"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/tsdb/chunks"
)

// disorder records where a tenant's source order breaks once labels are
// stripped. Its refs form strictly increasing runs, and a run starts at every
// series that discovery saw less than its predecessor. Both are nil while the
// tenant is in order.
type disorder struct {
	minStart labels.Labels // smallest run start (not counting the first run)
	maxEnd   labels.Labels // largest run end (not counting the last run)
}

// add records a series (out) that is less than its predecessor (prev).
func (d *disorder) add(prev, out labels.Labels) {
	if d.minStart == nil || labels.Compare(out, d.minStart) < 0 {
		d.minStart = out
	}
	if d.maxEnd == nil || labels.Compare(prev, d.maxEnd) > 0 {
		d.maxEnd = prev
	}
}

// labelCounts sums the output labels of a tenant's series and keeps the smallest
// and largest count per series, so sortTenant can size its arena up front.
type labelCounts struct {
	series, total, min, max int
}

func (c *labelCounts) add(n int) {
	if c.series == 0 || n < c.min {
		c.min = n
	}
	if n > c.max {
		c.max = n
	}
	c.series++
	c.total += n
}

// bound is an upper bound on the output labels that any n of the tenant's series
// hold together: the other series hold at least min each, and no series holds more
// than max. It is exact for the whole tenant.
func (c labelCounts) bound(n int) int {
	return min(c.total-(c.series-n)*c.min, c.max*n)
}

// sortTenant reorders refs in place by output labels and fails with
// errStreamDuplicateSeries on two equal output label sets. It returns the
// [lo, hi) range of refs it had to sort.
//
// Series less than every run start are all in the first run, and series greater
// than every run end are all in the last run, so both are already in place, and
// "less than minStart" and "greater than maxEnd" are monotone over refs. Two binary
// searches skip them, so a tenant with one displaced series only sorts the few
// series it moved across. Two equal series are both inside [lo, hi).
//
// The range is sorted by keys of symbol indexes: symIdx maps every output string
// of the tenant to its index in the sorted symbols, so slices.Compare on keys
// orders like labels.Compare. That holds 16 bytes per series of the range plus 8
// bytes per label, allocated once for labelCounts.bound of the range.
func sortTenant(ir tsdb.IndexReader, refs []storage.SeriesRef, d disorder, keep func(labels.Labels) (labels.Labels, bool), symbols []string, symIdx map[string]uint32, counts labelCounts) (lo, hi int, err error) {
	if uint64(2*counts.total) > math.MaxUint32 {
		return 0, 0, fmt.Errorf("%d output labels are too many to sort", counts.total)
	}
	var builder labels.ScratchBuilder
	var chks []chunks.Meta
	read := func(ref storage.SeriesRef) (labels.Labels, error) {
		if err := ir.Series(ref, &builder, &chks); err != nil {
			return nil, fmt.Errorf("series %d: %w", ref, err)
		}
		out, ok := keep(builder.Labels())
		if !ok {
			return nil, fmt.Errorf("series %d dropped by relabel on re-read: relabel is non-deterministic", ref)
		}
		return out, nil
	}
	compare := func(i int, to labels.Labels) int {
		if err != nil {
			return 0
		}
		var out labels.Labels
		out, err = read(refs[i])
		return labels.Compare(out, to)
	}
	lo = sort.Search(len(refs), func(i int) bool { return compare(i, d.minStart) >= 0 })
	hi = sort.Search(len(refs), func(i int) bool { return compare(i, d.maxEnd) > 0 })
	if err != nil {
		return lo, hi, err
	}
	if lo >= hi { // only if a re-read disagrees with discovery
		return lo, hi, fmt.Errorf("sort range [%d, %d) is empty: relabel is non-deterministic", lo, hi)
	}

	type key struct {
		ref      storage.SeriesRef
		off, end uint32 // the key is arena[off:end]
	}
	keys := make([]key, hi-lo)
	arena := make([]uint32, 0, 2*counts.bound(hi-lo)) // an upper bound, so append never grows it
	for i, ref := range refs[lo:hi] {
		out, err := read(ref)
		if err != nil {
			return lo, hi, err
		}
		keys[i] = key{ref: ref, off: uint32(len(arena))}
		for _, l := range out {
			n, nok := symIdx[l.Name]
			v, vok := symIdx[l.Value]
			if !nok || !vok {
				return lo, hi, fmt.Errorf("series %d: label %s=%q not seen at discovery: relabel is non-deterministic", ref, l.Name, l.Value)
			}
			arena = append(arena, n, v)
		}
		keys[i].end = uint32(len(arena))
	}
	slices.SortFunc(keys, func(a, b key) int { return slices.Compare(arena[a.off:a.end], arena[b.off:b.end]) })
	for i, k := range keys {
		if i > 0 && slices.Equal(arena[k.off:k.end], arena[keys[i-1].off:keys[i-1].end]) {
			b := labels.NewScratchBuilder(0)
			for j := k.off; j < k.end; j += 2 {
				b.Add(symbols[arena[j]], symbols[arena[j+1]])
			}
			return lo, hi, fmt.Errorf("%w: %s", errStreamDuplicateSeries, b.Labels())
		}
		refs[lo+i] = k.ref
	}
	return lo, hi, nil
}
