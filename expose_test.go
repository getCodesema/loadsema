package main

import (
	"strings"
	"testing"
)

func TestRenderMetrics(t *testing.T) {
	hist := make([]float64, len(waitBuckets)+1)
	for _, wait := range []float64{0.5, 3, 3, 90, 5000} {
		hist[bucketOf(wait)]++
	}
	page := renderMetrics(snapshot{
		enforce: true, running: 3, waiting: 2, cpuBudget: 26.5, ncpu: 24, diskFreeRatio: 0.18, cpuPressure: 0.34,
		admissions: 5, waitSum: 5096.5, waitHist: hist,
		profiles: map[string][]Profile{`42/e2e "chrome"`: {p(8.9)}},
		stats:    map[string]*jobStats{"42/unit/tests": {runs: 2, durationSec: 300, ioWriteBytes: 4096, oomKills: 1, last: runRecord{PidsPeak: 931}}},
	})
	for _, want := range []string{
		"# TYPE ci_gate_jobs gauge\n",
		`ci_gate_jobs{state="running"} 3` + "\n",
		`ci_gate_jobs{state="waiting"} 2` + "\n",
		"ci_gate_enforce 1\n",
		"ci_gate_cpu_budget_cores 26.5\n",
		"ci_gate_disk_free_ratio 0.18\n",
		"ci_gate_host_cpu_pressure_ratio 0.34\n",
		// Buckets are cumulative, and the count matches the admissions.
		`ci_gate_wait_seconds_bucket{le="1"} 1` + "\n",
		`ci_gate_wait_seconds_bucket{le="5"} 3` + "\n",
		`ci_gate_wait_seconds_bucket{le="300"} 4` + "\n",
		`ci_gate_wait_seconds_bucket{le="1200"} 4` + "\n",
		`ci_gate_wait_seconds_bucket{le="+Inf"} 5` + "\n",
		"ci_gate_wait_seconds_count 5\n",
		// Label values are escaped, and a job name keeps its own slashes.
		`ci_gate_job_profile_cores{project="42",ci_job="e2e \"chrome\""} 8.9` + "\n",
		`ci_gate_job_quota_cores{project="42",ci_job="e2e \"chrome\""} 14` + "\n",
		`ci_gate_job_runs_total{project="42",ci_job="unit/tests"} 2` + "\n",
		`ci_gate_job_last_pids_peak{project="42",ci_job="unit/tests"} 931` + "\n",
		`ci_gate_job_io_written_bytes_total{project="42",ci_job="unit/tests"} 4096` + "\n",
		`ci_gate_job_oom_kills_total{project="42",ci_job="unit/tests"} 1` + "\n",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("missing %q", want)
		}
	}
	unknown := renderMetrics(snapshot{diskFreeRatio: -1, cpuPressure: -1, waitHist: hist})
	if strings.Contains(unknown, "ci_gate_disk_free_ratio") || strings.Contains(unknown, "ci_gate_host_cpu_pressure_ratio") {
		t.Error("an unknown disk or pressure must not be exported")
	}
}
