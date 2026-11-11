# loadsema: load semaphore for GitLab Runner

A small regulator that decides, on a single host, which CI job may start now and which one waits.
It replaces fixed job caps (`concurrent`, `limit`) with a decision based on the real load of the host
and on what each job has used before.

Docker executor only. Go, standard library only, one static binary.

## Why

A runner cannot choose which pending job GitLab hands it, and `concurrent` counts jobs, not CPU or
memory: ten linters and ten end-to-end suites weigh the same. On one host this means either a low cap
that leaves the machine idle, or a high cap that overloads it.

The gate moves the queue onto the host. The runner accepts every job, each job waits at a gate before
it clones, and the regulator opens the gate for the job that fits.

## How it works

1. **Gate.** The runner's `pre_get_sources_script` runs `wait.sh`. The job writes a ticket
   `waiting/<job id>` in a shared directory and waits for `go/<job id>`.
2. **Real time.** Every 2 s the regulator reads `cpu.stat` and `memory.peak` in the cgroup of every
   container labelled `com.gitlab.gitlab-runner.job.id` (job and services), and the load of the whole
   host (`/proc/stat`, `/proc/meminfo`), so anything running outside CI counts too.
3. **History.** When a job ends, its cores (p90) and its memory peak are stored under
   `<project id>/<job name>`. The profile of a job is the median of its last 10 runs. A job never seen
   is sized like the median known job.
4. **Decision.** A job is admitted when the profiles of the running jobs plus its own fit the budget
   and the host is under 90% CPU. CPU budget: the host cores minus what runs outside CI. Memory
   budget: what is available while keeping 10% of the host free. Oldest first, else the biggest job
   that fits; a job that has waited 5 minutes blocks the ones behind it. When nothing runs, the oldest
   job always passes.
5. **Self-tuning.** Profiles are peaks, so their sum overstates real use. The CPU budget stretches
   while jobs wait on a host under 75% CPU and shrinks fast above 90% (`overcommit` in the log). No
   threshold to tune by hand.
6. **Quota per job.** A known job gets a CPU quota of 1.5 times its profile (2 cores at least, 90% of
   the host at most), set through the Docker API when it is admitted. Its tools then size their
   workers to what the job really uses instead of the whole host. The quota follows the job: if it
   grows, it runs into its quota, its profile rises, and so does its next quota.
7. **Low priority.** Every job container gets `CpuShares = 128`. When anything else on the host needs
   CPU, the kernel serves it first, at once, and the jobs take whatever is left.

The gate never blocks CI: a job passes on its own when the regulator has been silent for 30 s, or
after 20 minutes of waiting.

## Setup

Runner side, in `config.toml` (every `[[runners]]` entry that should go through the gate):

```toml
[[runners]]
  pre_get_sources_script = "[ -f /gate/wait.sh ] && sh /gate/wait.sh || true"
  [runners.docker]
    volumes = ["/srv/ci/gate:/gate"]
```

Set `concurrent` and `limit` high: they become runner slots, not a load cap (a waiting job holds a
slot).

Regulator side, next to the runner:

```yaml
services:
  regulator:
    build: https://github.com/getCodesema/loadsema.git
    restart: unless-stopped
    environment:
      CGROUP_ROOT: /host/cgroup
    volumes:
      - /srv/ci/gate:/gate
      - /var/run/docker.sock:/var/run/docker.sock
      - /sys/fs/cgroup:/host/cgroup:ro
```

The host cgroup tree is mounted aside because `/sys/fs/cgroup` inside a container only shows the
container itself. The regulator writes `wait.sh` into the gate directory when it starts.

## Configuration

All optional, read from the environment.

| Variable              | Default                | Meaning                                                       |
| --------------------- | ---------------------- | ------------------------------------------------------------- |
| `GATE_ENFORCE`        | `1`                    | `0` observes: every job passes at once, `wouldFit` is logged  |
| `GATE_DIR`            | `/gate`                | Shared directory (tickets, profiles, state)                   |
| `CGROUP_ROOT`         | `/sys/fs/cgroup`       | Host cgroup v2 tree                                           |
| `HOST_PROC`           | `/proc`                | Host `/proc`                                                  |
| `DOCKER_SOCKET`       | `/var/run/docker.sock` | Docker API socket                                             |
| `GATE_CPUS`           | host CPU count         | Cores CI may count on                                         |
| `GATE_AGING_SEC`      | `300`                  | Wait after which a job blocks the ones behind it              |
| `GATE_DEFAULT_CORES`  | `4`                    | Size of an unknown job while no profile exists at all         |
| `GATE_DEFAULT_MEM_GB` | `8`                    | Same, for memory                                              |
| `GATE_JOB_CPU_SHARES` | `128`                  | CPU priority of job containers (Docker default is 1024)       |
| `GATE_KEEP_RUNS`      | `10`                   | Runs kept per job                                             |
| `GATE_TICK_MS`        | `2000`                 | Loop period                                                   |
| `GATE_GRACE_MS`       | `10000`                | Time without containers after which a job is considered done  |

`wait.sh` reads `GATE_DIR` and `GATE_MAX_WAIT` (seconds, default `1200`) from the job environment.

## Files and log

In the gate directory: `waiting/` and `go/` (tickets), `profiles.json` (runs per job),
`state.json` (the self-tuned budget stretch), `alive` (heartbeat read by `wait.sh`), and
`runs.jsonl`.

`runs.jsonl` holds one JSON line per finished job with everything measured on it: wait and run
time, cores (p90 and average), quota, memory peak, bytes read and written, time held back by the
CPU quota, time stalled on CPU, IO and memory, memory limit hits, OOM kills, peak process count.
It is there to answer "why was this job slow" and to decide which resource deserves to enter the
decision. Admission does not read it. It is not rotated (about 400 bytes per job). Query it with
`jq`, or in SQL with DuckDB:

```sql
SELECT key, count(*) AS runs, avg(durationSec) AS sec, avg(throttledSec) AS throttled, max(ioStallSec) AS io
FROM read_json_auto('/srv/ci/gate/runs.jsonl') GROUP BY key ORDER BY sec DESC;
```

The regulator logs one JSON line per event on stdout:

- `start`: settings in use.
- `admit`: job, key, seconds waited, cores needed, quota given, jobs running and waiting, CPU budget,
  host CPU in use, `overcommit`, and three host readings kept for analysis only: CPU stolen by the
  hypervisor, IO pressure, free disk space.
- `done`: job, key, cores and memory recorded for this run.
- `error`: anything that failed.

## Metrics

The regulator serves Prometheus metrics on `:9273/metrics` (`GATE_METRICS_ADDR`, `off` to disable).
Any Prometheus-compatible collector can scrape it, OpenTelemetry collectors included.

- Live: `ci_gate_jobs{state}`, `ci_gate_cpu_budget_cores`, `ci_gate_cpu_committed_cores`,
  `ci_gate_cpu_used_cores`, `ci_gate_memory_budget_bytes`, `ci_gate_memory_used_bytes`,
  `ci_gate_host_cpu_busy_ratio`, `ci_gate_host_cpu_steal_ratio`, `ci_gate_host_io_pressure_ratio`,
  `ci_gate_disk_free_ratio`, `ci_gate_overcommit`, `ci_gate_enforce`.
- Gate: `ci_gate_admissions_total`, `ci_gate_wait_seconds` (histogram).
- Per job, labels `project` and `ci_job`: `ci_gate_job_profile_cores`,
  `ci_gate_job_profile_memory_bytes`, `ci_gate_job_quota_cores`, `ci_gate_job_runs_total`,
  `ci_gate_job_duration_seconds_total`, `ci_gate_job_wait_seconds_total`,
  `ci_gate_job_throttled_seconds_total`, `ci_gate_job_cpu_stall_seconds_total`,
  `ci_gate_job_io_stall_seconds_total`, and `ci_gate_job_last_*` for the last run.

Counters restart from zero with the regulator. The profile and quota gauges come from
`profiles.json`, so they are there from the first scrape.

A Grafana dashboard built on these metrics is in [`grafana/ci-gate.json`](grafana/ci-gate.json):
import it, or drop it in a provisioned dashboards folder. It uses the default Prometheus data source.

## Limits

- A waiting job shows as "running" in GitLab, and the wait counts in its timeout.
- Service containers start before the gate, so they stay up while the job waits.
- A job can write into the gate directory and admit itself: use it for runners that only serve
  projects you trust.
- Some Git strategies skip `pre_get_sources_script`: those jobs are measured but not held.
- The quota lands up to one loop period after a container starts.
- Tools must size themselves on `/sys/fs/cgroup/cpu.max`, not on the host core count.
- cgroup v2 only.

## Development

```sh
docker run --rm -v "$PWD":/src -w /src golang:1.27.4-alpine go test ./...
docker build -t loadsema .
```

`admission.go` holds the decisions as pure functions (`fits`, `pick`, `adapt`, `profileOf`,
`quotaOf`), covered by `admission_test.go`. `main.go` is the loop: Docker API, cgroups, tickets,
profiles, log. `metrics.go` reads the per-run measures, `expose.go` renders `/metrics`. The image
build runs the tests.

## License

[MIT](LICENSE)
