package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// counters are the cumulative cgroup counters of one container. They are recorded for analysis
// (runs.jsonl) and play no part in admission.
type counters struct {
	ioReadBytes, ioWriteBytes               float64
	throttledUsec                           float64 // time spent held back by the CPU quota
	cpuStallUsec, ioStallUsec, memStallUsec float64 // time some task waited for the resource
	memMaxEvents, oomKills                  float64 // memory limit hit, processes killed
	pidsPeak                                float64
}

func (c *counters) add(o counters) {
	c.ioReadBytes += o.ioReadBytes
	c.ioWriteBytes += o.ioWriteBytes
	c.throttledUsec += o.throttledUsec
	c.cpuStallUsec += o.cpuStallUsec
	c.ioStallUsec += o.ioStallUsec
	c.memStallUsec += o.memStallUsec
	c.memMaxEvents += o.memMaxEvents
	c.oomKills += o.oomKills
	c.pidsPeak += o.pidsPeak
}

// ioBytes sums the bytes read and written over every device of an io.stat file.
func ioBytes(stat string) (read, written float64) {
	for token := range strings.FieldsSeq(stat) {
		if v, ok := strings.CutPrefix(token, "rbytes="); ok {
			n, _ := strconv.ParseFloat(v, 64)
			read += n
		} else if v, ok := strings.CutPrefix(token, "wbytes="); ok {
			n, _ := strconv.ParseFloat(v, 64)
			written += n
		}
	}
	return read, written
}

// pressure returns one value of the "some" line of a pressure file: token is "total" (cumulative
// stall time in microseconds) or an average such as "avg10" (percent).
func pressure(content, token string) float64 {
	for line := range strings.SplitSeq(content, "\n") {
		rest, ok := strings.CutPrefix(line, "some ")
		if !ok {
			continue
		}
		for field := range strings.FieldsSeq(rest) {
			if v, ok := strings.CutPrefix(field, token+"="); ok {
				n, _ := strconv.ParseFloat(v, 64)
				return n
			}
		}
	}
	return 0
}

func readCounters(dir string) counters {
	read := func(name string) string {
		b, _ := os.ReadFile(filepath.Join(dir, name))
		return string(b)
	}
	var c counters
	c.ioReadBytes, c.ioWriteBytes = ioBytes(read("io.stat"))
	c.throttledUsec = field(read("cpu.stat"), "throttled_usec")
	c.cpuStallUsec = pressure(read("cpu.pressure"), "total")
	c.ioStallUsec = pressure(read("io.pressure"), "total")
	c.memStallUsec = pressure(read("memory.pressure"), "total")
	events := read("memory.events")
	c.memMaxEvents = field(events, "max")
	c.oomKills = field(events, "oom_kill")
	c.pidsPeak = readNum(filepath.Join(dir, "pids.peak"))
	return c
}

// runRecord is one line of runs.jsonl: everything measured on a finished job.
type runRecord struct {
	T            string   `json:"t"`
	Job          string   `json:"job"`
	Key          string   `json:"key"`
	WaitedSec    float64  `json:"waitedSec"`
	DurationSec  float64  `json:"durationSec"`
	Cores        float64  `json:"cores"` // p90, what the profile keeps
	CoresAvg     float64  `json:"coresAvg"`
	QuotaCores   *float64 `json:"quotaCores"`
	MemPeakBytes float64  `json:"memPeakBytes"`
	IOReadBytes  float64  `json:"ioReadBytes"`
	IOWriteBytes float64  `json:"ioWriteBytes"`
	ThrottledSec float64  `json:"throttledSec"`
	CPUStallSec  float64  `json:"cpuStallSec"`
	IOStallSec   float64  `json:"ioStallSec"`
	MemStallSec  float64  `json:"memStallSec"`
	MemMaxEvents float64  `json:"memMaxEvents"`
	OOMKills     float64  `json:"oomKills"`
	PidsPeak     float64  `json:"pidsPeak"`
}
