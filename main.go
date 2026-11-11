// Admission gate regulator for GitLab Runner (docker executor).
// Jobs drop a ticket in GATE_DIR/waiting (see wait.sh) and wait for GATE_DIR/go/<job id>.
// This loop measures every job through its cgroup, learns a profile per job name, and admits
// the waiting job that fits the room left on the host.
package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

//go:embed wait.sh
var waitScript []byte

const jobLabel = "com.gitlab.gitlab-runner.job.id"

func envStr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envNum(key string, fallback float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(key), 64); err == nil {
		return v
	}
	return fallback
}

type job struct {
	id       string
	key      string // "<project id>/<job name>", known once the ticket is read
	since    int64  // Unix milliseconds
	admitted bool
	lastSeen int64
	cpuUsec  map[string]float64 // per container, last cumulative CPU time
	memPeak  map[string]float64 // per container
	cores    float64
	memNow   float64
	samples  []float64
	capped   map[string]float64 // per container, the CPU quota already applied (0: priority only)

	admittedAt int64               // Unix milliseconds, 0 until admitted
	counters   map[string]counters // per container, last cumulative counters
}

type regulator struct {
	gate, cgroup, proc string
	docker             *http.Client
	enforce            bool
	graceMs, agingMs   int64
	keepRuns           int
	ncpu               float64
	fallback           Profile
	jobShares          int

	jobs       map[string]*job
	profiles   map[string][]Profile
	lastTick   int64
	lastBusy   float64 // cumulative busy and total CPU time of the host at the last tick
	lastTotal  float64
	lastSteal  float64
	stealRatio float64 // share of the host CPU time taken by the hypervisor over the last tick
	otherCores float64 // smoothed CPU used outside CI
	busyRatio  float64 // share of the host CPUs in use over the last tick
	overcommit float64 // self-tuned stretch of the CPU budget, see adapt(); kept in state.json

	// Behind the /metrics page, counted since the regulator started.
	admissions float64
	waitSum    float64
	waitHist   []float64 // per bucket of waitBuckets, last one is +Inf
	stats      map[string]*jobStats
	metrics    atomic.Value // string: the page rendered at the end of the last tick
}

func (r *regulator) path(elem ...string) string {
	return filepath.Join(append([]string{r.gate}, elem...)...)
}

// logEvent prints one JSON line: time, event name, then the fields of data in their order.
func logEvent(ev string, data any) {
	head, _ := json.Marshal(struct {
		T  string `json:"t"`
		Ev string `json:"ev"`
	}{time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), ev})
	body, err := json.Marshal(data)
	if err != nil || len(body) <= 2 {
		fmt.Println(string(head))
		return
	}
	fmt.Printf("%s,%s\n", head[:len(head)-1], body[1:])
}

func logError(format string, args ...any) {
	logEvent("error", struct {
		Message string `json:"message"`
	}{fmt.Sprintf(format, args...)})
}

func readNum(path string) float64 {
	b, _ := os.ReadFile(path)
	v, _ := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	return v
}

// field returns the number that follows key in a "key value" file such as cpu.stat or meminfo.
func field(content, key string) float64 {
	for line := range strings.SplitSeq(content, "\n") {
		if rest, ok := strings.CutPrefix(line, key); ok {
			if f := strings.Fields(rest); len(f) > 0 {
				v, _ := strconv.ParseFloat(f[0], 64)
				return v
			}
		}
	}
	return 0
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
func round2(v float64) float64 { return math.Round(v*100) / 100 }

// diskFreePct is the free space, in percent, of the filesystem holding the gate directory.
func (r *regulator) diskFreePct() int {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(r.gate, &fs); err != nil || fs.Blocks == 0 {
		return -1
	}
	return int(math.Round(float64(fs.Bavail) / float64(fs.Blocks) * 100))
}

func (r *regulator) job(id string, now int64) *job {
	j := r.jobs[id]
	if j == nil {
		j = &job{id: id, since: now, lastSeen: now, cpuUsec: map[string]float64{}, memPeak: map[string]float64{}, capped: map[string]float64{}, counters: map[string]counters{}}
		r.jobs[id] = j
	}
	return j
}

func (r *regulator) containersByJob() (map[string][]string, error) {
	filters, _ := json.Marshal(map[string][]string{"label": {jobLabel}})
	res, err := r.docker.Get("http://docker/containers/json?filters=" + url.QueryEscape(string(filters)))
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var list []struct {
		ID     string `json:"Id"`
		Labels map[string]string
	}
	if err := json.NewDecoder(res.Body).Decode(&list); err != nil {
		return nil, err
	}
	byJob := map[string][]string{}
	for _, c := range list {
		if id := c.Labels[jobLabel]; id != "" {
			byJob[id] = append(byJob[id], c.ID)
		}
	}
	return byJob, nil
}

func (r *regulator) cgroupDir(containerID string) string {
	systemd := filepath.Join(r.cgroup, "system.slice", "docker-"+containerID+".scope")
	if _, err := os.Stat(systemd); err == nil {
		return systemd
	}
	return filepath.Join(r.cgroup, "docker", containerID)
}

func (r *regulator) measure(j *job, containerIDs []string, dtSec float64) {
	var deltaUsec float64
	j.memNow = 0
	for _, cid := range containerIDs {
		dir := r.cgroupDir(cid)
		stat, err := os.ReadFile(filepath.Join(dir, "cpu.stat"))
		if err != nil {
			// Docker still lists the container but its cgroup is gone: keep what was last read
			// instead of overwriting the counters of the run with zeros.
			continue
		}
		usec := field(string(stat), "usage_usec")
		if prev, seen := j.cpuUsec[cid]; seen && usec >= prev {
			deltaUsec += usec - prev
		}
		j.cpuUsec[cid] = usec
		j.memNow += readNum(filepath.Join(dir, "memory.current"))
		j.memPeak[cid] = max(j.memPeak[cid], readNum(filepath.Join(dir, "memory.peak")))
		j.counters[cid] = readCounters(dir)
	}
	j.cores = 0
	if dtSec > 0 {
		j.cores = deltaUsec / 1e6 / dtSec
	}
	if j.admitted {
		j.samples = append(j.samples, j.cores)
	}
}

// cap gives every job container a low CPU priority: when anything else on the host needs CPU, the
// kernel serves it first, at once, and the jobs take whatever is left. A known job also gets its quota.
// TODO(ceiling): a container runs up to one tick with the runner's wide quota: react to docker
// events instead of the tick if a tool is seen sizing itself before the quota lands.
func (r *regulator) cap(j *job, containerIDs []string) {
	var cores float64
	if j.admitted && j.key != "" {
		cores, _ = quotaOf(r.profiles[j.key], r.ncpu)
	}
	for _, cid := range containerIDs {
		if applied, seen := j.capped[cid]; seen && applied == cores {
			continue
		}
		j.capped[cid] = cores
		update := map[string]int64{"CpuShares": int64(r.jobShares)}
		if cores > 0 {
			update["NanoCpus"] = int64(math.Round(cores * 1e9))
		}
		body, _ := json.Marshal(update)
		res, err := r.docker.Post("http://docker/containers/"+cid+"/update", "application/json", bytes.NewReader(body))
		if err != nil {
			logError("quota %.12s: %v", cid, err)
			continue
		}
		if res.StatusCode >= 300 {
			text, _ := io.ReadAll(res.Body)
			logError("quota %.12s: %d %s", cid, res.StatusCode, strings.TrimSpace(string(text)))
		}
		res.Body.Close()
	}
}

func writeJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path+".tmp", b, 0o644); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

// record appends everything measured on a finished job to runs.jsonl, for analysis only.
// TODO(ceiling): the file grows about 400 bytes per job and is never rotated: rotate it by size
// once it weighs on the disk.
func (r *regulator) record(j *job, run Profile) {
	var total counters
	for _, c := range j.counters {
		total.add(c)
	}
	var sum float64
	for _, s := range j.samples {
		sum += s
	}
	rec := runRecord{
		T:            time.Now().UTC().Format(time.RFC3339),
		Job:          j.id,
		Key:          j.key,
		WaitedSec:    float64(j.admittedAt-j.since) / 1000,
		DurationSec:  float64(j.lastSeen-j.admittedAt) / 1000,
		Cores:        round2(run.Cores),
		CoresAvg:     round2(sum / float64(len(j.samples))),
		MemPeakBytes: run.MemBytes,
		IOReadBytes:  total.ioReadBytes,
		IOWriteBytes: total.ioWriteBytes,
		ThrottledSec: round2(total.throttledUsec / 1e6),
		CPUStallSec:  round2(total.cpuStallUsec / 1e6),
		IOStallSec:   round2(total.ioStallUsec / 1e6),
		MemStallSec:  round2(total.memStallUsec / 1e6),
		MemMaxEvents: total.memMaxEvents,
		OOMKills:     total.oomKills,
		PidsPeak:     total.pidsPeak,
	}
	if q, ok := quotaOf(r.profiles[j.key], r.ncpu); ok {
		rec.QuotaCores = &q
	}
	st := r.stats[j.key]
	if st == nil {
		st = &jobStats{}
		r.stats[j.key] = st
	}
	st.runs++
	st.durationSec += rec.DurationSec
	st.waitSec += rec.WaitedSec
	st.throttledSec += rec.ThrottledSec
	st.cpuStallSec += rec.CPUStallSec
	st.ioStallSec += rec.IOStallSec
	st.last = rec

	line, _ := json.Marshal(rec)
	f, err := os.OpenFile(r.path("runs.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		logError("runs: %v", err)
		return
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		logError("runs: %v", err)
	}
}

func (r *regulator) finish(j *job) {
	if j.admitted && j.key != "" && len(j.samples) >= 3 {
		run := Profile{Cores: quantile(j.samples, 0.9)}
		for _, peak := range j.memPeak {
			run.MemBytes += peak
		}
		r.record(j, run)
		runs := append(r.profiles[j.key], run)
		r.profiles[j.key] = runs[max(0, len(runs)-r.keepRuns):]
		if err := writeJSON(r.path("profiles.json"), r.profiles); err != nil {
			logError("profiles: %v", err)
		}
		logEvent("done", struct {
			Job   string  `json:"job"`
			Key   string  `json:"key"`
			Cores float64 `json:"cores"`
			MemGb float64 `json:"memGb"`
		}{j.id, j.key, round1(run.Cores), round1(run.MemBytes / gib)})
	}
	os.Remove(r.path("waiting", j.id))
	os.Remove(r.path("go", j.id))
	delete(r.jobs, j.id)
}

const gib = 1 << 30

func (r *regulator) host(ciCores, ciMem float64, hasWaiting bool) Host {
	stat, _ := os.ReadFile(filepath.Join(r.proc, "stat"))
	first, _, _ := strings.Cut(string(stat), "\n")
	var total, idle, steal float64
	for i, f := range strings.Fields(first) {
		if i == 0 {
			continue // "cpu"
		}
		v, _ := strconv.ParseFloat(f, 64)
		total += v
		switch i {
		case 4, 5: // idle and iowait
			idle += v
		case 8: // taken by the hypervisor for other guests
			steal = v
		}
	}
	busy := total - idle
	if r.lastTotal > 0 && total > r.lastTotal {
		r.busyRatio = (busy - r.lastBusy) / (total - r.lastTotal)
		r.stealRatio = (steal - r.lastSteal) / (total - r.lastTotal)
		r.otherCores = 0.8*r.otherCores + 0.2*max(0, r.busyRatio*r.ncpu-ciCores)
		if tuned := adapt(r.overcommit, r.busyRatio, hasWaiting); tuned != r.overcommit {
			r.overcommit = tuned
			if err := writeJSON(r.path("state.json"), map[string]float64{"overcommit": tuned}); err != nil {
				logError("state: %v", err)
			}
		}
	}
	r.lastBusy, r.lastTotal, r.lastSteal = busy, total, steal
	meminfo, _ := os.ReadFile(filepath.Join(r.proc, "meminfo"))
	mem := func(key string) float64 { return field(string(meminfo), key+":") * 1024 }
	return Host{
		CPUBudget: (r.ncpu - r.otherCores) * r.overcommit,
		MemBudget: mem("MemAvailable") + ciMem - (1-saturated)*mem("MemTotal"),
		Saturated: r.busyRatio > saturated,
	}
}

func (r *regulator) tick() error {
	now := time.Now().UnixMilli()
	var dtSec float64
	if r.lastTick > 0 {
		dtSec = float64(now-r.lastTick) / 1000
	}
	r.lastTick = now
	if err := os.WriteFile(r.path("alive"), []byte(strconv.FormatInt(now, 10)), 0o644); err != nil {
		return err
	}

	tickets, err := os.ReadDir(r.path("waiting"))
	if err != nil {
		return err
	}
	for _, t := range tickets {
		j := r.job(t.Name(), now)
		if j.key != "" {
			continue
		}
		ticket, err := os.ReadFile(r.path("waiting", j.id))
		info, statErr := t.Info()
		if err != nil || statErr != nil {
			continue // removed since the listing
		}
		var project, name string
		for line := range strings.SplitSeq(string(ticket), "\n") {
			if v, ok := strings.CutPrefix(line, "project="); ok {
				project = v
			} else if v, ok := strings.CutPrefix(line, "name="); ok {
				name = v
			}
		}
		j.key = project + "/" + name
		j.since = info.ModTime().UnixMilli()
		// Its containers may have been seen before the ticket (services start first), and counted
		// as a job running without a gate: it waits from the ticket on.
		j.admittedAt = 0
		j.samples = nil
	}

	byJob, err := r.containersByJob()
	if err != nil {
		return err
	}
	for id, containerIDs := range byJob {
		j := r.job(id, now)
		// A job without ticket never went through the gate (hook skipped): it is already running.
		j.admitted = true
		admittedAt := now
		if j.key != "" {
			info, err := os.Stat(r.path("go", id))
			j.admitted = err == nil
			if err == nil {
				// The go file dates the admission, also for a job admitted before a restart.
				admittedAt = info.ModTime().UnixMilli()
			}
		}
		j.lastSeen = now
		if j.admitted && j.admittedAt == 0 {
			j.admittedAt = admittedAt
		}
		r.measure(j, containerIDs, dtSec)
		r.cap(j, containerIDs)
	}
	for _, j := range r.jobs {
		if now-j.lastSeen > r.graceMs {
			r.finish(j)
		}
	}

	// A job never seen before is sized like the median known job.
	known := make([]Profile, 0, len(r.profiles))
	for _, runs := range r.profiles {
		known = append(known, profileOf(runs, r.fallback))
	}
	unknown := profileOf(known, r.fallback)
	need := func(j *job) Profile {
		if j.key == "" {
			return Profile{}
		}
		return profileOf(r.profiles[j.key], unknown)
	}

	var running []Running
	var waiting []*job
	var ciCores, ciMem float64
	for _, j := range r.jobs {
		ciCores += j.cores
		ciMem += j.memNow
		if j.admitted {
			running = append(running, Running{Expected: need(j), Actual: Profile{Cores: j.cores, MemBytes: j.memNow}})
		} else {
			waiting = append(waiting, j)
		}
	}
	h := r.host(ciCores, ciMem, len(waiting) > 0)
	nRunning, nWaiting := len(running), len(waiting)
	ioPressureFile, _ := os.ReadFile(filepath.Join(r.proc, "pressure", "io"))
	ioPressure := string(ioPressureFile)

	admit := func(j *job, wouldFit *bool) {
		if err := os.WriteFile(r.path("go", j.id), nil, 0o644); err != nil {
			logError("admit %s: %v", j.id, err)
			return
		}
		j.admitted = true
		waited := float64(now-j.since) / 1000
		r.admissions++
		r.waitSum += waited
		r.waitHist[bucketOf(waited)]++
		var quota *float64
		if q, ok := quotaOf(r.profiles[j.key], r.ncpu); ok {
			quota = &q
		}
		logEvent("admit", struct {
			Job        string   `json:"job"`
			Key        string   `json:"key"`
			WaitedSec  int64    `json:"waitedSec"`
			NeedCores  float64  `json:"needCores"`
			QuotaCores *float64 `json:"quotaCores"`
			Running    int      `json:"running"`
			Waiting    int      `json:"waiting"`
			CPUBudget  float64  `json:"cpuBudget"`
			BusyPct    int      `json:"busyPct"`
			Overcommit float64  `json:"overcommit"`
			// Recorded for analysis, not used by the decision.
			StealPct    float64 `json:"stealPct"`
			IOPressure  float64 `json:"ioPressure"`
			DiskFreePct int     `json:"diskFreePct"`
			WouldFit    *bool   `json:"wouldFit,omitempty"`
		}{j.id, j.key, int64(math.Round(float64(now-j.since) / 1000)), round1(need(j).Cores), quota, nRunning, nWaiting,
			round1(h.CPUBudget), int(math.Round(r.busyRatio * 100)), round2(r.overcommit),
			round1(r.stealRatio * 100), pressure(ioPressure, "avg10"), r.diskFreePct(), wouldFit})
	}
	if r.enforce {
		// One admission per tick: the next decision sees this job as committed.
		queue := make([]Waiting, len(waiting))
		for i, j := range waiting {
			queue[i] = Waiting{ID: j.id, Since: j.since, Need: need(j)}
		}
		if id, ok := pick(queue, running, h, r.agingMs, now); ok {
			admit(r.jobs[id], nil)
		}
	} else {
		// Observe mode: admit at once, record what the gate would have decided.
		for _, j := range waiting {
			wouldFit := fits(need(j), running, h)
			admit(j, &wouldFit)
		}
	}

	var committed float64
	for _, run := range running {
		committed += max(run.Expected.Cores, run.Actual.Cores)
	}
	diskFree := float64(r.diskFreePct()) / 100
	r.metrics.Store(renderMetrics(snapshot{
		enforce: r.enforce, running: nRunning, waiting: nWaiting,
		cpuBudget: h.CPUBudget, cpuCommitted: committed, cpuUsed: ciCores,
		memBudget: h.MemBudget, memUsed: ciMem,
		busyRatio: r.busyRatio, stealRatio: r.stealRatio, overcommit: r.overcommit,
		ioPressure: pressure(ioPressure, "avg10"), diskFreeRatio: diskFree,
		admissions: r.admissions, waitSum: r.waitSum, waitHist: r.waitHist,
		profiles: r.profiles, stats: r.stats, ncpu: r.ncpu,
	}))
	return nil
}

func main() {
	sock := envStr("DOCKER_SOCKET", "/var/run/docker.sock")
	r := &regulator{
		gate:   envStr("GATE_DIR", "/gate"),
		cgroup: envStr("CGROUP_ROOT", "/sys/fs/cgroup"),
		proc:   envStr("HOST_PROC", "/proc"),
		docker: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sock)
			},
		}},
		enforce:    os.Getenv("GATE_ENFORCE") != "0",
		graceMs:    int64(envNum("GATE_GRACE_MS", 10_000)),
		agingMs:    int64(envNum("GATE_AGING_SEC", 300) * 1000),
		keepRuns:   int(envNum("GATE_KEEP_RUNS", 10)),
		ncpu:       envNum("GATE_CPUS", float64(runtime.NumCPU())),
		fallback:   Profile{Cores: envNum("GATE_DEFAULT_CORES", 4), MemBytes: envNum("GATE_DEFAULT_MEM_GB", 8) * gib},
		jobShares:  int(envNum("GATE_JOB_CPU_SHARES", 128)), // docker default is 1024: other containers win about 5 to 1
		jobs:       map[string]*job{},
		profiles:   map[string][]Profile{},
		overcommit: 1,
		waitHist:   make([]float64, len(waitBuckets)+1),
		stats:      map[string]*jobStats{},
	}
	tick := time.Duration(envNum("GATE_TICK_MS", 2000)) * time.Millisecond

	for _, dir := range []string{"waiting", "go"} {
		if err := os.MkdirAll(r.path(dir), 0o777); err != nil {
			logError("%v", err)
			os.Exit(1)
		}
		os.Chmod(r.path(dir), 0o777) // jobs of any user drop their ticket here
	}
	if err := os.WriteFile(r.path("wait.sh"), waitScript, 0o644); err != nil {
		logError("%v", err)
		os.Exit(1)
	}
	if b, err := os.ReadFile(r.path("profiles.json")); err == nil {
		if err := json.Unmarshal(b, &r.profiles); err != nil {
			logError("profiles.json ignored: %v", err)
		}
	}
	if b, err := os.ReadFile(r.path("state.json")); err == nil {
		var state struct {
			Overcommit float64 `json:"overcommit"`
		}
		if json.Unmarshal(b, &state) == nil && state.Overcommit > 0 {
			r.overcommit = state.Overcommit
		}
	}

	type limits struct {
		AgingMs int64 `json:"agingMs"`
	}
	logEvent("start", struct {
		Enforce  bool    `json:"enforce"`
		Cpus     float64 `json:"cpus"`
		Limits   limits  `json:"limits"`
		Fallback Profile `json:"fallback"`
	}{r.enforce, r.ncpu, limits{r.agingMs}, r.fallback})

	// The page is rendered by the loop and only read here, so the handler shares no state with it.
	if addr := envStr("GATE_METRICS_ADDR", ":9273"); addr != "off" {
		http.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
			page, _ := r.metrics.Load().(string)
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
			io.WriteString(w, page)
		})
		go func() {
			if err := http.ListenAndServe(addr, nil); err != nil {
				logError("metrics: %v", err)
			}
		}()
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	for {
		if err := r.tick(); err != nil {
			logError("%v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(tick):
		}
	}
}
