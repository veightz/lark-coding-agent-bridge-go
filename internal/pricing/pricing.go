package pricing

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

type Rates struct {
	Input     float64
	Output    float64
	CacheRead float64
}

type entry struct {
	prefix  string
	rates   Rates
	peakMul float64
}

var registry []entry

func register(prefix string, r Rates, peakMul float64) {
	registry = append(registry, entry{prefix: strings.ToLower(prefix), rates: r, peakMul: peakMul})
}

func init() {
	register("deepseek/deepseek-chat", Rates{Input: 0.5, Output: 2.0, CacheRead: 0.1}, 2.0)
	register("deepseek-chat", Rates{Input: 0.5, Output: 2.0, CacheRead: 0.1}, 2.0)
	register("deepseek/deepseek-reasoner", Rates{Input: 1.0, Output: 4.0, CacheRead: 0.25}, 2.0)
	register("deepseek-reasoner", Rates{Input: 1.0, Output: 4.0, CacheRead: 0.25}, 2.0)
	register("deepseek/deepseek-r1", Rates{Input: 1.0, Output: 4.0, CacheRead: 0.25}, 2.0)

	register("claude-sonnet-4-", Rates{Input: 22.0, Output: 110.0, CacheRead: 2.2}, 0)
	register("claude-sonnet-4", Rates{Input: 22.0, Output: 110.0, CacheRead: 2.2}, 0)
	register("claude-3.5-sonnet", Rates{Input: 22.0, Output: 110.0, CacheRead: 2.2}, 0)
	register("claude-3-haiku", Rates{Input: 5.8, Output: 29.2, CacheRead: 0.6}, 0)
	register("claude-opus-4-", Rates{Input: 110.0, Output: 440.0, CacheRead: 11.0}, 0)
	register("claude-opus-4", Rates{Input: 110.0, Output: 440.0, CacheRead: 11.0}, 0)

	register("gpt-4o-mini", Rates{Input: 1.1, Output: 4.4, CacheRead: 0.55}, 0)
	register("gpt-4o", Rates{Input: 18.3, Output: 73.0, CacheRead: 9.1}, 0)

	// Sort by prefix length descending so longer (more specific) prefixes match first.
	sort.Slice(registry, func(i, j int) bool {
		return len(registry[i].prefix) > len(registry[j].prefix)
	})
}

func peakBeijing(t time.Time) bool {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		loc = time.FixedZone("CST", 8*3600)
	}
	beijing := t.In(loc)
	h, m := beijing.Hour(), beijing.Minute()
	if h >= 16 && h < 24 {
		return true
	}
	if h == 0 && m < 30 {
		return true
	}
	return false
}

func Lookup(model string) (Rates, float64, bool) {
	lower := strings.ToLower(model)
	for _, e := range registry {
		if strings.HasPrefix(lower, e.prefix) {
			return e.rates, e.peakMul, true
		}
	}
	return Rates{}, 0, false
}

func Calculate(model string, inputTokens, outputTokens, cachedInputTokens int, now time.Time) (cny float64, label string) {
	r, peakMul, ok := Lookup(model)
	if !ok {
		return 0, ""
	}

	multiplier := 1.0
	if peakMul > 0 && peakBeijing(now) {
		multiplier = peakMul
	}

	// Claude nests cache under input (cached ≤ input). OpenCode reports
	// non-cached input separately, so cached may exceed input — treat
	// input as already-non-cached in that case.
	nonCachedInput := inputTokens
	if cachedInputTokens > 0 && cachedInputTokens <= inputTokens {
		nonCachedInput = inputTokens - cachedInputTokens
	}

	cost := (float64(nonCachedInput)*r.Input +
		float64(cachedInputTokens)*r.CacheRead +
		float64(outputTokens)*r.Output) / 1_000_000 * multiplier

	cost = math.Round(cost*10000) / 10000

	label = fmt.Sprintf("¥%.4f", cost)
	if peakMul > 0 && multiplier > 1 {
		label += " (高峰2x)"
	}

	return cost, label
}

func FormatCNY(cost float64) string {
	if cost <= 0 {
		return ""
	}
	return fmt.Sprintf("¥%.4f", cost)
}
