package main

import (
	"math"
	"testing"
)

const gb = 1 << 30

var testHost = Host{CPUBudget: 20, MemBudget: 60 * gb}

const aging = 300_000

// p is a profile of the given cores and gigabytes (2 by default).
func p(cores float64, gbs ...float64) Profile {
	mem := 2.0
	if len(gbs) > 0 {
		mem = gbs[0]
	}
	return Profile{Cores: cores, MemBytes: mem * gb}
}

func run(expected Profile) Running { return Running{Expected: expected} }

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestEmptyHostAdmitsAnything(t *testing.T) {
	saturatedHost := Host{CPUBudget: 20, MemBudget: 60 * gb, Saturated: true}
	if !fits(p(64, 500), nil, saturatedHost) {
		t.Error("a job bigger than the budget must pass when nothing runs")
	}
}

func TestFitsNeedsCPUMemoryAndAnUnsaturatedHost(t *testing.T) {
	two := []Running{run(p(8)), run(p(8))}
	cases := []struct {
		name    string
		need    Profile
		running []Running
		host    Host
		want    bool
	}{
		{"room left", p(4), two, testHost, true},
		{"no CPU left", p(5), two, testHost, false},
		{"no memory left", p(1, 30), []Running{run(p(1, 40))}, testHost, false},
		{"saturated host", p(1), []Running{run(p(1))}, Host{CPUBudget: 20, MemBudget: 60 * gb, Saturated: true}, false},
	}
	for _, c := range cases {
		if got := fits(c.need, c.running, c.host); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRunningJobCountsForWhatItUses(t *testing.T) {
	greedy := []Running{{Expected: p(2), Actual: p(18, 1)}}
	if fits(p(4), greedy, testHost) {
		t.Error("actual use above the profile must count")
	}
}

func TestOldestGoesFirstWhenItFits(t *testing.T) {
	queue := []Waiting{{"young", 2000, p(1)}, {"old", 1000, p(4)}}
	if id, ok := pick(queue, []Running{run(p(8))}, testHost, aging, 3000); !ok || id != "old" {
		t.Errorf("got %q %v, want old", id, ok)
	}
}

func TestBiggestFittingJobBackfills(t *testing.T) {
	queue := []Waiting{{"old", 1000, p(16)}, {"small", 2000, p(2)}, {"medium", 3000, p(6)}}
	if id, ok := pick(queue, []Running{run(p(8))}, testHost, aging, 4000); !ok || id != "medium" {
		t.Errorf("got %q %v, want medium", id, ok)
	}
}

func TestAgedJobBlocksBackfill(t *testing.T) {
	queue := []Waiting{{"old", 0, p(16)}, {"small", 2000, p(2)}}
	if id, ok := pick(queue, []Running{run(p(8))}, testHost, aging, 400_000); ok {
		t.Errorf("got %q, want nothing admitted", id)
	}
}

func TestProfileIsMedianOrFallback(t *testing.T) {
	if got := profileOf([]Profile{p(2, 1), p(9, 8), p(4, 3)}, p(4, 8)); got != p(4, 3) {
		t.Errorf("median: got %+v", got)
	}
	if got := profileOf(nil, p(4, 8)); got != p(4, 8) {
		t.Errorf("fallback: got %+v", got)
	}
}

func TestQuotaFollowsTheProfile(t *testing.T) {
	cases := []struct {
		name  string
		runs  []Profile
		want  float64
		known bool
	}{
		{"headroom over the usual peak", []Profile{p(8.9), p(9.2), p(8.8)}, 14, true}, // ceil(8.9 * 1.5)
		{"floor", []Profile{p(0.2)}, 2, true},
		{"never above the host share", []Profile{p(20)}, 21.6, true},
		{"unknown job", nil, 0, false},
		{"no run yet", []Profile{}, 0, false},
	}
	for _, c := range cases {
		got, known := quotaOf(c.runs, 24)
		if known != c.known || !near(got, c.want) {
			t.Errorf("%s: got %v %v, want %v %v", c.name, got, known, c.want, c.known)
		}
	}
}

func TestBudgetStretchesWhenIdleAndShrinksOnSaturation(t *testing.T) {
	cases := []struct {
		name       string
		overcommit float64
		busy       float64
		waiting    bool
		want       float64
	}{
		{"jobs wait on an idle host", 1, 0.5, true, 1.05},
		{"nobody waits", 1, 0.5, false, 1},
		{"busy enough", 1, 0.8, true, 1},
		{"saturated", 2, 0.95, true, 1.8},
		{"floor", 0.5, 0.95, true, 0.5},
		{"ceiling", 4, 0.5, true, 4},
	}
	for _, c := range cases {
		if got := adapt(c.overcommit, c.busy, c.waiting); !near(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
