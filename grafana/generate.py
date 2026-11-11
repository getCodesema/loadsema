"""Generates ci-gate.json, the committed dashboard: python3 grafana/generate.py grafana/ci-gate.json"""
import json
import sys

DS = {"type": "prometheus"}
JOB = '{project=~"$project"}'


def target(expr, legend=None, ref="A", table=False):
    t = {"expr": expr, "refId": ref, "datasource": DS}
    if legend:
        t["legendFormat"] = legend
    if table:
        t.update({"format": "table", "instant": True})
    return t


def stat(title, expr, x, unit="none", steps=None, decimals=None, w=3, description=None, mappings=None):
    defaults = {"unit": unit, "thresholds": {"mode": "absolute", "steps": steps or [{"color": "green", "value": None}]}}
    if decimals is not None:
        defaults["decimals"] = decimals
    p = {
        "type": "stat", "title": title, "datasource": DS,
        "gridPos": {"h": 4, "w": w, "x": x, "y": 0},
        "targets": [target(expr)],
        "fieldConfig": {"defaults": defaults, "overrides": []},
        "options": {"colorMode": "value", "graphMode": "area", "reduceOptions": {"calcs": ["lastNotNull"]}},
    }
    if description:
        p["description"] = description
    if mappings:
        p["fieldConfig"]["defaults"]["mappings"] = mappings
    return p


def series(title, targets, x, y, unit="none", w=12, h=9, description=None, bars=False, maxv=None):
    custom = {"fillOpacity": 10, "lineWidth": 1, "showPoints": "never"}
    if bars:
        custom = {"drawStyle": "bars", "fillOpacity": 60, "lineWidth": 0}
    defaults = {"unit": unit, "min": 0, "custom": custom}
    if maxv is not None:
        defaults["max"] = maxv
    p = {
        "type": "timeseries", "title": title, "datasource": DS,
        "gridPos": {"h": h, "w": w, "x": x, "y": y},
        "targets": targets,
        "fieldConfig": {"defaults": defaults, "overrides": []},
        "options": {
            "legend": {"displayMode": "table", "placement": "bottom", "calcs": ["lastNotNull", "max"]},
            "tooltip": {"mode": "multi", "sort": "desc"},
        },
    }
    if description:
        p["description"] = description
    return p


ok, warn, bad = "green", "orange", "red"
panels = [
    stat("Gate mode", "ci_gate_enforce", 0, mappings=[{"type": "value", "options": {
        "1": {"text": "holding jobs", "color": ok}, "0": {"text": "observing only", "color": warn}}}]),
    stat("Jobs running", 'ci_gate_jobs{state="running"}', 3),
    stat("Jobs waiting in the gate", 'ci_gate_jobs{state="waiting"}', 6,
         steps=[{"color": ok, "value": None}, {"color": warn, "value": 1}, {"color": bad, "value": 8}]),
    stat("Host CPU pressure", "ci_gate_host_cpu_pressure_ratio", 9, unit="percentunit", decimals=0,
         steps=[{"color": ok, "value": None}, {"color": warn, "value": 0.3}, {"color": bad, "value": 0.5}],
         description="Share of the last 10 s some task waited for a CPU. The gate widens its budget under 30% "
                     "and admits nothing above 50%."),
    stat("Gate wait, p95 over 1 h", "histogram_quantile(0.95, sum by (le) (increase(ci_gate_wait_seconds_bucket[1h])))", 12,
         unit="s", decimals=0,
         steps=[{"color": ok, "value": None}, {"color": warn, "value": 60}, {"color": bad, "value": 300}]),
    stat("Budget stretch", "ci_gate_overcommit", 15, decimals=2,
         description="Self-tuned: grows while jobs wait on a host with idle CPU, shrinks on saturation."),
    stat("Disk free", "ci_gate_disk_free_ratio", 18, unit="percentunit", decimals=0,
         steps=[{"color": bad, "value": None}, {"color": warn, "value": 0.1}, {"color": ok, "value": 0.2}]),
    stat("Jobs admitted (time range)", "increase(ci_gate_admissions_total[$__range])", 21, decimals=0),

    series("CPU: budget, committed and used (cores)", [
        target("ci_gate_cpu_budget_cores", "budget", "A"),
        target("ci_gate_cpu_committed_cores", "committed (profiles of running jobs)", "B"),
        target("ci_gate_cpu_used_cores", "used by job containers", "C"),
        target("ci_gate_host_cpus", "host cores", "D"),
    ], 0, 4, description="A job is admitted while committed plus its profile stays under the budget."),
    series("Jobs running and waiting", [
        target('ci_gate_jobs{state="running"}', "running", "A"),
        target('ci_gate_jobs{state="waiting"}', "waiting", "B"),
    ], 12, 4),

    series("Gate wait (15 min window)", [
        target("histogram_quantile(0.5, sum by (le) (increase(ci_gate_wait_seconds_bucket[15m])))", "p50", "A"),
        target("histogram_quantile(0.95, sum by (le) (increase(ci_gate_wait_seconds_bucket[15m])))", "p95", "B"),
    ], 0, 13, unit="s"),
    series("Admissions per 5 min", [target("increase(ci_gate_admissions_total[5m])", "jobs admitted", "A")],
           12, 13, bars=True),

    series("Host: CPU pressure, CPU in use, CPU stolen, disk pressure", [
        target("ci_gate_host_cpu_pressure_ratio", "time waiting for a CPU (what the gate decides on)", "A"),
        target("ci_gate_host_cpu_busy_ratio", "CPU in use", "B"),
        target("ci_gate_host_cpu_steal_ratio", "CPU taken by the hypervisor", "C"),
        target("ci_gate_host_io_pressure_ratio", "time waiting for the disk", "D"),
    ], 0, 22, unit="percentunit", maxv=1),
    series("Memory: budget and used by jobs", [
        target("ci_gate_memory_budget_bytes", "budget", "A"),
        target("ci_gate_memory_used_bytes", "used by job containers", "B"),
    ], 12, 22, unit="bytes"),
]

RUNS = f"increase(ci_gate_job_runs_total{JOB}[$__range])"
TIME = f"increase(ci_gate_job_duration_seconds_total{JOB}[$__range])"


def total(name):
    return f"increase(ci_gate_job_{name}{JOB}[$__range])"


def table(title, description, y, columns, sort, shares=()):
    """columns: (ref, expr, name, unit, decimals). shares: names coloured by how much time is lost."""
    overrides = []
    for _, _, name, unit, decimals in columns:
        props = [{"id": "unit", "value": unit}, {"id": "decimals", "value": decimals}]
        if name in shares:
            props += [
                {"id": "custom.cellOptions", "value": {"type": "color-text"}},
                {"id": "thresholds", "value": {"mode": "absolute", "steps": [
                    {"color": ok, "value": None}, {"color": warn, "value": 0.1}, {"color": bad, "value": 0.25}]}},
            ]
        overrides.append({"matcher": {"id": "byName", "options": name}, "properties": props})
    return {
        "type": "table", "title": title, "datasource": DS, "description": description,
        "gridPos": {"h": 11, "w": 24, "x": 0, "y": y},
        "targets": [target(expr, ref=ref, table=True) for ref, expr, _, _, _ in columns],
        "transformations": [
            {"id": "merge", "options": {}},
            {"id": "organize", "options": {
                "excludeByName": {"Time": True, "__name__": True, "instance": True, "job": True},
                "renameByName": {"ci_job": "Job", "project": "Project",
                                 **{f"Value #{ref}": name for ref, _, name, _, _ in columns}},
            }},
        ],
        "fieldConfig": {"defaults": {"custom": {"align": "auto", "filterable": True}}, "overrides": overrides},
        "options": {"sortBy": [{"displayName": sort, "desc": True}], "footer": {"show": False}},
    }


SINCE = "Counted over the selected time range, on runs finished while the regulator was up."
panels.append(table("Jobs: size and time", "Profile and quota are what the gate uses now. " + SINCE, 31, [
    ("A", f"ci_gate_job_profile_cores{JOB}", "Profile (cores)", "none", 1),
    ("B", f"ci_gate_job_quota_cores{JOB}", "Quota (cores)", "none", 0),
    ("C", f"ci_gate_job_last_cores{JOB}", "Cores, last run (p90)", "none", 1),
    ("D", f"ci_gate_job_profile_memory_bytes{JOB}", "Profile (memory)", "bytes", 1),
    ("E", f"ci_gate_job_last_memory_bytes{JOB}", "Memory peak, last run", "bytes", 1),
    ("F", f"ci_gate_job_last_pids_peak{JOB}", "Peak processes, last run", "none", 0),
    ("G", RUNS, "Runs", "none", 0),
    ("H", f"{TIME} / {RUNS}", "Avg duration", "s", 0),
    ("I", f"{total('wait_seconds_total')} / {RUNS}", "Avg gate wait", "s", 0),
], "Profile (cores)"))
LOST = ["Held back by quota", "Waiting for CPU", "Waiting for disk", "Waiting for memory"]
panels.append(table("Jobs: what holds them back", "Share of the run time a job lost, and what it did to the disk and "
                    "the memory. " + SINCE, 42, [
    ("A", f"{total('throttled_seconds_total')} / {TIME}", LOST[0], "percentunit", 1),
    ("B", f"{total('cpu_stall_seconds_total')} / {TIME}", LOST[1], "percentunit", 1),
    ("C", f"{total('io_stall_seconds_total')} / {TIME}", LOST[2], "percentunit", 1),
    ("D", f"{total('memory_stall_seconds_total')} / {TIME}", LOST[3], "percentunit", 1),
    ("E", f"{total('io_read_bytes_total')} / {RUNS}", "Disk read per run", "bytes", 1),
    ("F", f"{total('io_written_bytes_total')} / {RUNS}", "Disk written per run", "bytes", 1),
    ("G", total("memory_limit_hits_total"), "Memory limit hits", "none", 0),
    ("H", total("oom_kills_total"), "Killed for lack of memory", "none", 0),
], LOST[1], shares=LOST))

panels += [
    series("Duration of the last run, per job", [target(f"ci_gate_job_last_duration_seconds{JOB}", "{{ci_job}}")],
           0, 53, unit="s"),
    series("Cores of the last run (p90), per job", [target(f"ci_gate_job_last_cores{JOB}", "{{ci_job}}")], 12, 53),
    series("Memory peak of the last run, per job", [target(f"ci_gate_job_last_memory_bytes{JOB}", "{{ci_job}}")],
           0, 62, unit="bytes"),
    series("Peak processes of the last run, per job", [target(f"ci_gate_job_last_pids_peak{JOB}", "{{ci_job}}")], 12, 62),
]
for i, p in enumerate(panels, start=1):
    p["id"] = i

dashboard = {
    "uid": "ci-gate",
    "title": "loadsema",
    "description": "Admission gate of the GitLab runners, by loadsema (github.com/getCodesema/loadsema).",
    "tags": ["ci"],
    "timezone": "browser",
    "refresh": "30s",
    "time": {"from": "now-6h", "to": "now"},
    "schemaVersion": 39,
    "templating": {"list": [{
        "name": "project", "label": "Project", "type": "query", "datasource": DS,
        "query": {"query": "label_values(ci_gate_job_profile_cores, project)", "refId": "A"},
        "refresh": 2, "multi": True, "includeAll": True, "allValue": ".*",
        "current": {"text": "All", "value": "$__all"},
    }]},
    "panels": panels,
}
json.dump(dashboard, open(sys.argv[1], "w"), indent=2, ensure_ascii=False)
open(sys.argv[1], "a").write("\n")
