package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIOBytesSumsEveryDevice(t *testing.T) {
	stat := "8:0 rbytes=100 wbytes=32817152 rios=0 wios=8024 dbytes=0 dios=0\n" +
		"8:16 rbytes=50 wbytes=8 rios=1 wios=1 dbytes=0 dios=0\n"
	if read, written := ioBytes(stat); read != 150 || written != 32817160 {
		t.Errorf("got %v read, %v written", read, written)
	}
	if read, written := ioBytes(""); read != 0 || written != 0 {
		t.Errorf("empty file: got %v, %v", read, written)
	}
}

func TestPressureReadsTheSomeLine(t *testing.T) {
	content := "some avg10=2.17 avg60=0.39 avg300=0.08 total=252666\n" +
		"full avg10=1.81 avg60=0.32 avg300=0.06 total=226304\n"
	if got := pressure(content, "total"); got != 252666 {
		t.Errorf("total: got %v", got)
	}
	if got := pressure(content, "avg10"); got != 2.17 {
		t.Errorf("avg10: got %v", got)
	}
	if got := pressure("", "total"); got != 0 {
		t.Errorf("missing file: got %v", got)
	}
}

func TestFieldDoesNotConfuseEventNames(t *testing.T) {
	events := "low 0\nhigh 0\nmax 3\noom 1\noom_kill 2\noom_group_kill 0\n"
	if got := field(events, "max"); got != 3 {
		t.Errorf("max: got %v", got)
	}
	if got := field(events, "oom_kill"); got != 2 {
		t.Errorf("oom_kill: got %v", got)
	}
}

func TestMeasureKeepsCountersWhenTheCgroupIsGone(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "docker", "abc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("cpu.stat", "usage_usec 1000\nthrottled_usec 500\n")
	write("pids.peak", "42\n")
	r := &regulator{cgroup: root}
	j := &job{cpuUsec: map[string]float64{}, memPeak: map[string]float64{}, counters: map[string]counters{}}
	r.measure(j, []string{"abc"}, 2)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	r.measure(j, []string{"abc"}, 2)
	if c := j.counters["abc"]; c.pidsPeak != 42 || c.throttledUsec != 500 {
		t.Errorf("counters lost: %+v", c)
	}
}
