package main

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// waitBuckets are the upper bounds, in seconds, of the gate wait histogram.
var waitBuckets = []float64{1, 5, 15, 60, 300, 1200}

// bucketOf is the index in waitBuckets of the bucket a wait falls in, or len(waitBuckets) above all.
func bucketOf(sec float64) int {
	for i, upper := range waitBuckets {
		if sec <= upper {
			return i
		}
	}
	return len(waitBuckets)
}

// jobStats is what the gate has seen of one job key since the regulator started.
type jobStats struct {
	runs                                                        float64
	durationSec, waitSec, throttledSec, cpuStallSec, ioStallSec float64
	memStallSec, ioReadBytes, ioWriteBytes                      float64
	memMaxEvents, oomKills                                      float64
	last                                                        runRecord
}

// snapshot is everything the /metrics page shows, taken at the end of a tick.
type snapshot struct {
	enforce               bool
	running, waiting      int
	cpuBudget             float64 // cores CI may commit
	cpuCommitted          float64 // cores the running jobs are counted for
	cpuUsed               float64 // cores the job containers use right now
	memBudget, memUsed    float64
	busyRatio, stealRatio float64
	cpuPressure           float64 // share of the last 10 s some task waited for a CPU, -1 when unknown
	overcommit            float64
	ioPressure            float64 // percent, avg10
	diskFreeRatio         float64 // -1 when unknown
	admissions            float64
	waitSum               float64
	waitHist              []float64 // per bucket, not cumulative; last one is +Inf
	profiles              map[string][]Profile
	stats                 map[string]*jobStats
	ncpu                  float64
}

type metricWriter struct{ b strings.Builder }

func (w *metricWriter) family(name, kind, help string) {
	fmt.Fprintf(&w.b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, kind)
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

// sample writes one line. labels are name, value pairs.
func (w *metricWriter) sample(name string, v float64, labels ...string) {
	w.b.WriteString(name)
	for i := 0; i+1 < len(labels); i += 2 {
		if i == 0 {
			w.b.WriteByte('{')
		} else {
			w.b.WriteByte(',')
		}
		fmt.Fprintf(&w.b, `%s="%s"`, labels[i], labelEscaper.Replace(labels[i+1]))
	}
	if len(labels) >= 2 {
		w.b.WriteByte('}')
	}
	w.b.WriteByte(' ')
	w.b.WriteString(strconv.FormatFloat(v, 'g', -1, 64))
	w.b.WriteByte('\n')
}

func (w *metricWriter) gauge(name, help string, v float64) {
	w.family(name, "gauge", help)
	w.sample(name, v)
}

// jobLabels splits a "<project id>/<job name>" key into the labels of a per-job series.
func jobLabels(key string) []string {
	project, name, _ := strings.Cut(key, "/")
	return []string{"project", project, "ci_job", name}
}

// renderMetrics writes the snapshot in the Prometheus text exposition format.
func renderMetrics(s snapshot) string {
	w := &metricWriter{}
	enforce := 0.0
	if s.enforce {
		enforce = 1
	}
	w.gauge("ci_gate_enforce", "1 when the gate holds jobs, 0 when it only observes.", enforce)

	w.family("ci_gate_jobs", "gauge", "Jobs seen by the gate, by state.")
	w.sample("ci_gate_jobs", float64(s.running), "state", "running")
	w.sample("ci_gate_jobs", float64(s.waiting), "state", "waiting")

	w.gauge("ci_gate_cpu_budget_cores", "Cores CI may commit: host cores minus use outside CI, stretched by the overcommit.", s.cpuBudget)
	w.gauge("ci_gate_cpu_committed_cores", "Cores the running jobs are counted for: the higher of their profile and their use.", s.cpuCommitted)
	w.gauge("ci_gate_cpu_used_cores", "Cores the job containers use right now.", s.cpuUsed)
	w.gauge("ci_gate_host_cpus", "Cores of the host.", s.ncpu)
	w.gauge("ci_gate_memory_budget_bytes", "Memory CI may commit.", s.memBudget)
	w.gauge("ci_gate_memory_used_bytes", "Memory the job containers use right now.", s.memUsed)
	w.gauge("ci_gate_host_cpu_busy_ratio", "Share of the host CPUs in use over the last tick.", s.busyRatio)
	w.gauge("ci_gate_host_cpu_steal_ratio", "Share of the host CPU time taken by the hypervisor over the last tick.", s.stealRatio)
	if s.cpuPressure >= 0 {
		w.gauge("ci_gate_host_cpu_pressure_ratio", "Share of the last 10 s some task waited for a CPU: what the gate decides on.", s.cpuPressure)
	}
	w.gauge("ci_gate_host_io_pressure_ratio", "Share of the last 10 s some task waited for the disk.", s.ioPressure/100)
	if s.diskFreeRatio >= 0 {
		w.gauge("ci_gate_disk_free_ratio", "Free space of the filesystem holding the gate directory.", s.diskFreeRatio)
	}
	w.gauge("ci_gate_overcommit", "Self-tuned stretch of the CPU budget.", s.overcommit)

	w.family("ci_gate_admissions_total", "counter", "Jobs admitted since the regulator started.")
	w.sample("ci_gate_admissions_total", s.admissions)
	w.family("ci_gate_wait_seconds", "histogram", "Time jobs spent waiting in the gate.")
	var cumulative float64
	for i, upper := range waitBuckets {
		cumulative += s.waitHist[i]
		w.sample("ci_gate_wait_seconds_bucket", cumulative, "le", strconv.FormatFloat(upper, 'g', -1, 64))
	}
	w.sample("ci_gate_wait_seconds_bucket", s.admissions, "le", "+Inf")
	w.sample("ci_gate_wait_seconds_sum", s.waitSum)
	w.sample("ci_gate_wait_seconds_count", s.admissions)

	keys := make([]string, 0, len(s.profiles))
	for key := range s.profiles {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	perJob := func(name, kind, help string, keys []string, value func(key string) (float64, bool)) {
		w.family(name, kind, help)
		for _, key := range keys {
			if v, ok := value(key); ok {
				w.sample(name, v, jobLabels(key)...)
			}
		}
	}
	perJob("ci_gate_job_profile_cores", "gauge", "Learned CPU profile of a job: median of its last runs.", keys,
		func(k string) (float64, bool) { return profileOf(s.profiles[k], Profile{}).Cores, true })
	perJob("ci_gate_job_profile_memory_bytes", "gauge", "Learned memory profile of a job: median of its last runs.", keys,
		func(k string) (float64, bool) { return profileOf(s.profiles[k], Profile{}).MemBytes, true })
	perJob("ci_gate_job_quota_cores", "gauge", "CPU quota a job gets when admitted.", keys,
		func(k string) (float64, bool) { return quotaOf(s.profiles[k], s.ncpu) })

	ran := make([]string, 0, len(s.stats))
	for key := range s.stats {
		ran = append(ran, key)
	}
	slices.Sort(ran)
	stat := func(pick func(*jobStats) float64) func(string) (float64, bool) {
		return func(k string) (float64, bool) { return pick(s.stats[k]), true }
	}
	perJob("ci_gate_job_runs_total", "counter", "Finished runs of a job since the regulator started.", ran,
		stat(func(j *jobStats) float64 { return j.runs }))
	perJob("ci_gate_job_duration_seconds_total", "counter", "Run time of a job, summed over its runs.", ran,
		stat(func(j *jobStats) float64 { return j.durationSec }))
	perJob("ci_gate_job_wait_seconds_total", "counter", "Gate wait of a job, summed over its runs.", ran,
		stat(func(j *jobStats) float64 { return j.waitSec }))
	perJob("ci_gate_job_throttled_seconds_total", "counter", "Time a job was held back by its CPU quota, summed over its runs.", ran,
		stat(func(j *jobStats) float64 { return j.throttledSec }))
	perJob("ci_gate_job_cpu_stall_seconds_total", "counter", "Time a job waited for CPU, summed over its runs.", ran,
		stat(func(j *jobStats) float64 { return j.cpuStallSec }))
	perJob("ci_gate_job_io_stall_seconds_total", "counter", "Time a job waited for the disk, summed over its runs.", ran,
		stat(func(j *jobStats) float64 { return j.ioStallSec }))
	perJob("ci_gate_job_memory_stall_seconds_total", "counter", "Time a job waited for memory, summed over its runs.", ran,
		stat(func(j *jobStats) float64 { return j.memStallSec }))
	perJob("ci_gate_job_io_read_bytes_total", "counter", "Bytes a job read from disk, summed over its runs.", ran,
		stat(func(j *jobStats) float64 { return j.ioReadBytes }))
	perJob("ci_gate_job_io_written_bytes_total", "counter", "Bytes a job wrote to disk, summed over its runs.", ran,
		stat(func(j *jobStats) float64 { return j.ioWriteBytes }))
	perJob("ci_gate_job_memory_limit_hits_total", "counter", "Times a job reached its memory limit, summed over its runs.", ran,
		stat(func(j *jobStats) float64 { return j.memMaxEvents }))
	perJob("ci_gate_job_oom_kills_total", "counter", "Processes of a job killed for lack of memory, summed over its runs.", ran,
		stat(func(j *jobStats) float64 { return j.oomKills }))
	perJob("ci_gate_job_last_duration_seconds", "gauge", "Run time of the last run of a job.", ran,
		stat(func(j *jobStats) float64 { return j.last.DurationSec }))
	perJob("ci_gate_job_last_cores", "gauge", "Cores (p90) of the last run of a job.", ran,
		stat(func(j *jobStats) float64 { return j.last.Cores }))
	perJob("ci_gate_job_last_memory_bytes", "gauge", "Memory peak of the last run of a job.", ran,
		stat(func(j *jobStats) float64 { return j.last.MemPeakBytes }))
	perJob("ci_gate_job_last_pids_peak", "gauge", "Peak process count of the last run of a job.", ran,
		stat(func(j *jobStats) float64 { return j.last.PidsPeak }))
	return w.b.String()
}
