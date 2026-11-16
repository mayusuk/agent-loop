package runners

import (
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// procTable lists the host's live processes from /proc, without zombies.
func procTable() (map[int]procInfo, error) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	out := make(map[int]procInfo, len(ents))
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue // it has ended
		}
		// The command name is in parentheses and may hold spaces.
		i := strings.LastIndexByte(string(b), ')')
		if i < 0 {
			continue
		}
		f := strings.Fields(string(b[i+1:]))
		// f[0] state, f[1] ppid, f[19] starttime (fields 3, 4 and 22).
		if len(f) < 20 || f[0] == "Z" || f[0] == "X" {
			continue
		}
		ppid, _ := strconv.Atoi(f[1])
		start, _ := strconv.ParseInt(f[19], 10, 64)
		out[pid] = procInfo{ppid: ppid, start: start}
	}
	return out, nil
}

// setSubreaper makes this process the child subreaper of its descendants:
// a process whose parent ends is re-parented here, not to init, so the
// supervisor still finds and stops it.
func setSubreaper() error {
	return unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0)
}
