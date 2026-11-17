package runners

import "golang.org/x/sys/unix"

// sZomb is a zombie's p_stat (sys/proc.h).
const sZomb = 5

// procTable lists the host's live processes with sysctl, without zombies.
func procTable() (map[int]procInfo, error) {
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	out := make(map[int]procInfo, len(kps))
	for _, kp := range kps {
		if kp.Proc.P_stat == sZomb {
			continue
		}
		st := kp.Proc.P_starttime
		out[int(kp.Proc.P_pid)] = procInfo{ppid: int(kp.Eproc.Ppid), start: int64(st.Sec)*1e6 + int64(st.Usec)}
	}
	return out, nil
}

// setSubreaper does nothing: macOS has no child subreaper, so a process
// whose parent ends goes to launchd and is found only if a scan saw it
// first (work plan D20).
func setSubreaper() error { return nil }
