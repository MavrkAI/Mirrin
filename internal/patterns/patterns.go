// Package patterns finds recurring themes in what the user asks, deterministically,
// so the model only has to phrase an offer, not spot the pattern.
package patterns

import (
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

var stop = map[string]bool{}

func init() {
	for _, w := range strings.Fields("a an the is are was were be been do does did can could would should will i me my you your it its this that these those of to in on at for with and or but not no so if then what whats when where who why how which any some please just now right there here up out about from by as into over again still also very really tell give get let make set run check show me one two three") {
		stop[w] = true
	}
}

var reWord = regexp.MustCompile(`\p{L}[\p{L}'-]*`) // any script, not just ASCII

// words returns the content words of a request.
func words(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range reWord.FindAllString(strings.ToLower(s), -1) {
		w = strings.Trim(w, "'-")
		if utf8.RuneCountInString(w) < 3 || stop[w] {
			continue
		}
		// crude stemming: headlines→headline, stories→story-ish
		w = strings.TrimSuffix(w, "s")
		out[w] = true
	}
	return out
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for w := range a {
		if b[w] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	return float64(inter) / float64(union)
}

// Cluster is a group of similar requests.
type Cluster struct {
	Key      string   // the most shared word
	Requests []string // the original texts
}

// Find groups requests by shared content words and returns clusters with at
// least min members, largest first. A cluster is a theme worth an offer.
func Find(requests []string, min int) []Cluster {
	type item struct {
		text string
		w    map[string]bool
	}
	items := make([]item, 0, len(requests))
	for _, r := range requests {
		if w := words(r); len(w) > 0 {
			items = append(items, item{r, w})
		}
	}
	used := make([]bool, len(items))
	var clusters []Cluster
	for i := range items {
		if used[i] {
			continue
		}
		members := []int{i}
		for j := i + 1; j < len(items); j++ {
			if used[j] {
				continue
			}
			// similar to any member: shares ≥1 content word with Jaccard ≥ 0.2, or shares two words
			for _, m := range members {
				shared := 0
				for w := range items[m].w {
					if items[j].w[w] {
						shared++
					}
				}
				if shared >= 2 || (shared >= 1 && jaccard(items[m].w, items[j].w) >= 0.2) {
					members = append(members, j)
					break
				}
			}
		}
		if len(members) < min {
			continue
		}
		counts := map[string]int{}
		var c Cluster
		for _, m := range members {
			used[m] = true
			c.Requests = append(c.Requests, items[m].text)
			for w := range items[m].w {
				counts[w]++
			}
		}
		best, bestN := "", 0
		for w, n := range counts {
			if n > bestN || (n == bestN && w < best) {
				best, bestN = w, n
			}
		}
		c.Key = best
		clusters = append(clusters, c)
	}
	sort.SliceStable(clusters, func(i, j int) bool { return len(clusters[i].Requests) > len(clusters[j].Requests) })
	return clusters
}
