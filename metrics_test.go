package main

import "testing"

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
