// Package analytics — stats.go
// All statistical functions in this file operate exclusively on anonymous []int
// slices (message counts). No user identifiers are present at this layer.
// ZKA: By the time any function here is called, the hashedID→count map has
// already been destroyed by Pipeline.FlushAndDestroy().
package analytics

import (
	"math"
	"sort"
)

// CalculateGini computes the Gini coefficient for a distribution of message
// counts. The Gini coefficient ranges from 0 (perfect equality — everyone sends
// the same number of messages) to 1 (maximal inequality — one person sends all
// messages). Values above ~0.6 in a channel may indicate burnout concentration.
//
// Algorithm: Sort ascending, compute cumulative share of population vs.
// cumulative share of total messages, then use the area-under-Lorenz-curve
// method: G = 1 - 2*B where B is the area under the Lorenz curve.
//
// ZKA: `counts` is an anonymous []int — no user identity information present.
func CalculateGini(counts []int) float64 {
	if len(counts) == 0 {
		return 0.0 // edge case: no data → no inequality
	}

	// Work on a copy to avoid mutating the caller's slice.
	sorted := make([]int, len(counts))
	copy(sorted, counts)
	sort.Ints(sorted) // ascending: lowest count first

	n := len(sorted)
	total := 0
	for _, v := range sorted {
		total += v
	}
	if total == 0 {
		return 0.0 // edge case: all zeros → treat as perfectly equal
	}

	// Standard Gini accumulation formula:
	// G = (2 * Σ (i * x_i)) / (n * Σ x_i)  - (n+1)/n
	// where i is 1-based rank.
	numerator := 0
	for i, v := range sorted {
		numerator += (i + 1) * v // (i+1) is the 1-based rank
	}

	gini := (2.0*float64(numerator))/(float64(n)*float64(total)) - float64(n+1)/float64(n)

	// Clamp to [0, 1] to guard against floating-point drift.
	if gini < 0 {
		gini = 0
	}
	if gini > 1 {
		gini = 1
	}
	return gini
}

// CalculateParetoTop20 computes the percentage of total messages contributed
// by the top 20% most active senders. A value near 80 indicates a classic
// Pareto distribution; very high values signal workload concentration risk.
//
// Returns a percentage in [0, 100].
//
// ZKA: `counts` is an anonymous []int — no user identity information present.
func CalculateParetoTop20(counts []int) float64 {
	if len(counts) == 0 {
		return 0.0
	}

	// Work on a copy to avoid mutating the caller's slice.
	sorted := make([]int, len(counts))
	copy(sorted, counts)
	sort.Sort(sort.Reverse(sort.IntSlice(sorted))) // descending: highest first

	total := 0
	for _, v := range sorted {
		total += v
	}
	if total == 0 {
		return 0.0 // all senders have zero messages → undefined, return 0
	}

	// Take the ceiling of 20% of participants to ensure at least 1 person is
	// included even in small channels.
	top20Count := int(math.Ceil(float64(len(sorted)) * 0.20))
	if top20Count < 1 {
		top20Count = 1
	}
	// Guard against slice bounds overshoot (shouldn't happen after Ceil, but
	// defensive programming is correct here).
	if top20Count > len(sorted) {
		top20Count = len(sorted)
	}

	topSum := 0
	for i := 0; i < top20Count; i++ {
		topSum += sorted[i]
	}

	return (float64(topSum) / float64(total)) * 100.0
}

// CalculateZScore computes how many standard deviations the current channel
// volume is from the historical mean. A z-score above +2 or +3 may indicate
// an unusual spike (potential crisis/incident), while a strongly negative score
// may indicate disengagement.
//
// Returns 0.0 when historicalStdDev is zero to avoid division-by-zero panics.
//
// ZKA: This function receives only scalar integers/floats — no PII whatsoever.
func CalculateZScore(currentVolume int, historicalMean float64, historicalStdDev float64) float64 {
	if historicalStdDev == 0.0 {
		// Division by zero guard: if there is no historical variance we cannot
		// compute a meaningful z-score; return neutral 0.0.
		return 0.0
	}
	return (float64(currentVolume) - historicalMean) / historicalStdDev
}
