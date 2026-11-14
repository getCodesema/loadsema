package main

import (
	"cmp"
	"math"
	"slices"
)

// Profile is what a job needs: CPU cores and memory.
type Profile struct {
	Cores    float64 `json:"cores"`
	MemBytes float64 `json:"memBytes"`
}

// Waiting is a job held in the gate. Since is in Unix milliseconds.
type Waiting struct {
	ID    string
	Since int64
	Need  Profile
}

// Running is an admitted job: what its history predicts and what it uses right now.
type Running struct {
	Expected Profile
	Actual   Profile
}

// Host is the room CI may use on the machine.
type Host struct {
	CPUBudget float64
	MemBudget float64
	Saturated bool
}

// saturated is the share of the host memory that CI may fill, and of the host CPUs a quota may reach.
const saturated = 0.9

// Load is how hard the host CPUs are pressed, and the two marks it is read against: under Calm
// there is room for more, above High the jobs already slow each other down.
type Load struct {
	Value, Calm, High float64
}

// pressureLoad reads the load from the kernel's CPU pressure: the share of the last 10 s some task
// waited for a CPU. It says whether jobs wait, where the share of CPUs in use only says how much
// they run: 80% in use is comfortable on one host and painful on another. Marks measured on a
// 24-core host: about 0.25 with one pipeline running alone, 0.5 to 0.6 when jobs took half longer.
func pressureLoad(waiting float64) Load { return Load{Value: waiting, Calm: 0.30, High: 0.50} }

// busyLoad is the fallback on a kernel without pressure information: the share of CPUs in use.
func busyLoad(busyRatio float64) Load { return Load{Value: busyRatio, Calm: 0.75, High: saturated} }

// fits reports whether a job fits the room left on a host that is not saturated.
func fits(need Profile, running []Running, h Host) bool {
	if len(running) == 0 {
		return true // never deadlock on a job bigger than the budget
	}
	var cores, mem float64
	for _, r := range running {
		cores += max(r.Expected.Cores, r.Actual.Cores)
		mem += max(r.Expected.MemBytes, r.Actual.MemBytes)
	}
	return !h.Saturated && cores+need.Cores <= h.CPUBudget && mem+need.MemBytes <= h.MemBudget
}

// pick returns the job to admit: the oldest when it fits, else the biggest job that fits,
// unless the oldest has waited longer than agingMs.
func pick(waiting []Waiting, running []Running, h Host, agingMs, now int64) (string, bool) {
	if len(waiting) == 0 {
		return "", false
	}
	queue := slices.Clone(waiting)
	slices.SortStableFunc(queue, func(a, b Waiting) int { return int(a.Since - b.Since) })
	oldest := queue[0]
	if fits(oldest.Need, running, h) {
		return oldest.ID, true
	}
	if now-oldest.Since > agingMs {
		return "", false // hold the room for the starving job
	}
	var best *Waiting
	for i := range queue[1:] {
		w := &queue[i+1]
		if fits(w.Need, running, h) && (best == nil || w.Need.Cores > best.Need.Cores) {
			best = w
		}
	}
	if best == nil {
		return "", false
	}
	return best.ID, true
}

// adapt tunes the stretch of the CPU budget (additive increase, multiplicative decrease).
// Profiles are peaks, so their sum overstates real use: stretch the budget while jobs wait on a
// calm host, shrink it when the host is pressed. settled says the running jobs have shown their
// load: a job spends its first minute installing things, and a host that looks calm then is no
// proof there is room for more.
func adapt(overcommit float64, l Load, hasWaiting, settled bool) float64 {
	if l.Value > l.High {
		return max(0.5, overcommit*0.9)
	}
	if hasWaiting && settled && l.Value < l.Calm {
		return min(4, overcommit+0.05)
	}
	return overcommit
}

func quantile(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	return s[min(len(s)-1, int(q*float64(len(s))))]
}

// profileOf is the median of a job's past runs, or the fallback for a job never seen.
func profileOf(runs []Profile, fallback Profile) Profile {
	if len(runs) == 0 {
		return fallback
	}
	cores := make([]float64, len(runs))
	mem := make([]float64, len(runs))
	for i, r := range runs {
		cores[i], mem[i] = r.Cores, r.MemBytes
	}
	return Profile{Cores: quantile(cores, 0.5), MemBytes: quantile(mem, 0.5)}
}

// quotaOf is the CPU quota of a job, in cores: headroom over its highest recent peak, so its tools
// size their workers to what the job really uses instead of the whole host. The highest run, not
// the median: runs that stop early use little and must not lower the quota of the full ones, and a
// run held at its quota records that quota, so the very next one gets 1.5 times more.
// A job never seen has no quota of its own: it keeps the runner's wide one until it is learned.
func quotaOf(runs []Profile, ncpu float64) (float64, bool) {
	if len(runs) == 0 {
		return 0, false
	}
	highest := slices.MaxFunc(runs, func(a, b Profile) int { return cmp.Compare(a.Cores, b.Cores) })
	cores := math.Ceil(highest.Cores * 1.5)
	return min(ncpu*saturated, max(2, cores)), true
}
