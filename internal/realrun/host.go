package realrun

import (
	"bufio"
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// CPUModel returns the machine's CPU model name as the operating system
// reports it (Windows registry, /proc/cpuinfo on Linux, sysctl on macOS),
// or a generic description when it cannot be read.
func CPUModel() string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var name string
	switch runtime.GOOS {
	case "windows":
		out, err := exec.CommandContext(ctx, "reg", "query",
			`HKLM\HARDWARE\DESCRIPTION\System\CentralProcessor\0`, "/v", "ProcessorNameString").Output()
		if err == nil {
			name = parseRegString(string(out))
		}
	case "linux":
		name = cpuinfoModel("/proc/cpuinfo")
	case "darwin":
		if out, err := exec.CommandContext(ctx, "sysctl", "-n", "machdep.cpu.brand_string").Output(); err == nil {
			name = strings.TrimSpace(string(out))
		}
	}
	if name == "" {
		return fmt.Sprintf("unknown CPU (%s/%s)", runtime.GOOS, runtime.GOARCH)
	}
	return fmt.Sprintf("%s (%d logical CPUs)", name, runtime.NumCPU())
}

var regStringRE = regexp.MustCompile(`ProcessorNameString\s+REG_SZ\s+(.+)`)

// parseRegString extracts the value from `reg query ... /v
// ProcessorNameString` output.
func parseRegString(out string) string {
	m := regStringRE.FindStringSubmatch(out)
	if m == nil {
		return ""
	}
	return strings.Join(strings.Fields(m[1]), " ")
}

// cpuinfoModel reads the first "model name" line of a /proc/cpuinfo file.
func cpuinfoModel(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if ok && strings.TrimSpace(k) == "model name" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// CPUPerformancePct samples Windows' "% Processor Performance" counter: the
// CPU's current effective frequency as a percentage of its nominal one
// (above 100 under turbo). It returns NaN where the counter cannot be read
// (other platforms, or typeperf missing). CPU temperature is not sampled:
// Windows exposes the CPU package sensor only to administrators, and the
// ACPI thermal zones readable without that are not CPU sensors.
func CPUPerformancePct() float64 {
	if runtime.GOOS != "windows" {
		return math.NaN()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "typeperf", `\Processor Information(_Total)\% Processor Performance`, "-sc", "1").Output()
	if err != nil {
		return math.NaN()
	}
	return parseTypeperf(string(out))
}

// parseTypeperf reads the value of the first sample row of typeperf's CSV
// output ("time","value"); NaN if there is none.
func parseTypeperf(out string) float64 {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(strings.TrimSpace(line), `","`)
		if len(fields) != 2 || strings.Contains(line, "PDH-CSV") {
			continue
		}
		if v, err := strconv.ParseFloat(strings.Trim(fields[1], `"`), 64); err == nil {
			return v
		}
	}
	return math.NaN()
}
