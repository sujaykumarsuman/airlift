package beam

import (
	"math"

	"rsc.io/qr/coding"
)

// penalty scores a symbol by the four mask-evaluation rules of ISO/IEC 18004
// §8.8.2; the reference encoder picks the mask that minimises it and the page's
// encoder (qrjs.js) carries the same rules. rsc.io/qr/coding takes an
// explicit mask and does no evaluation of its own, so this supplies it.
func penalty(code *coding.Code) int {
	n := code.Size
	g := make([][]bool, n)
	dark := 0
	for y := 0; y < n; y++ {
		g[y] = make([]bool, n)
		for x := 0; x < n; x++ {
			if code.Black(x, y) {
				g[y][x] = true
				dark++
			}
		}
	}
	return rule1(g) + rule2(g) + rule3(g) + rule4(dark, n*n)
}

// rule1: runs of five or more same-colour modules in a row or column score
// 3 + (run − 5) each.
func rule1(g [][]bool) int {
	n := len(g)
	score := 0
	run := func(at func(i int) bool) {
		count := 1
		for i := 1; i < n; i++ {
			if at(i) == at(i-1) {
				count++
				continue
			}
			if count >= 5 {
				score += 3 + (count - 5)
			}
			count = 1
		}
		if count >= 5 {
			score += 3 + (count - 5)
		}
	}
	for y := 0; y < n; y++ {
		row := g[y]
		run(func(i int) bool { return row[i] })
	}
	for x := 0; x < n; x++ {
		run(func(i int) bool { return g[i][x] })
	}
	return score
}

// rule2: each 2×2 block of one colour scores 3.
func rule2(g [][]bool) int {
	n := len(g)
	score := 0
	for y := 0; y < n-1; y++ {
		for x := 0; x < n-1; x++ {
			v := g[y][x]
			if g[y][x+1] == v && g[y+1][x] == v && g[y+1][x+1] == v {
				score += 3
			}
		}
	}
	return score
}

// finder-like 1:1:3:1:1 patterns with four light modules to one side.
var patA = []bool{true, false, true, true, true, false, true, false, false, false, false}
var patB = []bool{false, false, false, false, true, false, true, true, true, false, true}

// rule3: every occurrence of either pattern in a row or column scores 40.
func rule3(g [][]bool) int {
	n := len(g)
	score := 0
	count := func(at func(i int) bool) int {
		c := 0
		for i := 0; i+11 <= n; i++ {
			if matches(at, i, patA) || matches(at, i, patB) {
				c++
			}
		}
		return c
	}
	for y := 0; y < n; y++ {
		row := g[y]
		score += 40 * count(func(i int) bool { return row[i] })
	}
	for x := 0; x < n; x++ {
		score += 40 * count(func(i int) bool { return g[i][x] })
	}
	return score
}

func matches(at func(i int) bool, start int, pat []bool) bool {
	for j, want := range pat {
		if at(start+j) != want {
			return false
		}
	}
	return true
}

// rule4: deviation of the dark-module proportion from 50%, in 5% steps, scores
// 10 per step.
func rule4(dark, total int) int {
	if total == 0 {
		return 0
	}
	percent := float64(dark) * 100 / float64(total)
	return 10 * int(math.Abs(percent-50)/5)
}
