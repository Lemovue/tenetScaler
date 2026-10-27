//go:build windows

package llama

import (
	"syscall"
	"testing"
)

// TestChildInKillJobFromCreation checks that the launch window is closed:
// after a launch the launching process (here the test binary) is itself in
// the kill-on-close job, so every later child is a member from the moment
// it is created, and the launched child is a member.
func TestChildInKillJobFromCreation(t *testing.T) {
	p := launch(t, fakeLauncher(t), "job-1", 0)
	if p.jobErr != nil {
		t.Fatalf("orphan protection: %v", p.jobErr)
	}
	job, err := jobHandle()
	if err != nil {
		t.Fatal(err)
	}
	self, err := syscall.GetCurrentProcess()
	if err != nil {
		t.Fatal(err)
	}
	if in, err := inJob(self, job); err != nil || !in || !killJob.selfIn {
		t.Fatalf("launching process in the kill-on-close job: %v, %v (selfIn %v)", in, err, killJob.selfIn)
	}
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(p.PID()))
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.CloseHandle(h)
	if in, err := inJob(h, job); err != nil || !in {
		t.Fatalf("replica in the kill-on-close job: %v, %v", in, err)
	}
}

func TestParseTasklist(t *testing.T) {
	tests := []struct {
		name, out string
		want      []int
		err       bool
	}{
		{"none (English)", "INFO: No tasks are running which match the specified criteria.\r\n", nil, false},
		{"none (localised)", "INFORMATION: Es werden keine Aufgaben mit den angegebenen Kriterien ausgeführt.\r\n", nil, false},
		{"empty", "", nil, false},
		{"two servers", "\"llama-server.exe\",\"1234\",\"Console\",\"1\",\"52,140 K\"\r\n\"LLAMA-SERVER.EXE\",\"88\",\"Console\",\"1\",\"1,024 K\"\r\n", []int{1234, 88}, false},
		{"other image ignored", "\"notepad.exe\",\"7\",\"Console\",\"1\",\"9 K\"\r\n", nil, false},
		{"bad pid", "\"llama-server.exe\",\"x\",\"Console\",\"1\",\"9 K\"\r\n", nil, true},
	}
	for _, tc := range tests {
		got, err := parseTasklist([]byte(tc.out))
		if tc.err {
			if err == nil {
				t.Errorf("%s: no error", tc.name)
			}
			continue
		}
		if err != nil || len(got) != len(tc.want) {
			t.Errorf("%s: %v, %v; want %v", tc.name, got, err, tc.want)
			continue
		}
		for _, pid := range tc.want {
			if !got[pid] {
				t.Errorf("%s: pid %d missing from %v", tc.name, pid, got)
			}
		}
	}
}
