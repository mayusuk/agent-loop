package runners

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

// SuperviseCommand is the hidden agent-loop subcommand the local runner
// starts every handler under (work plan D20):
// `agent-loop supervise <grace-ms> <path> -- <argv...>`.
const SuperviseCommand = "supervise"

// procPoll is how often the supervisor scans for the handler's processes.
const procPoll = 25 * time.Millisecond

// procInfo is one process in the host's process table. start tells a
// process from a later one that reuses its pid.
type procInfo struct {
	ppid  int
	start int64
}

// SuperviseMain runs a handler and makes sure that, when it ends, every
// process it started ends too, including one that left the process group
// with setsid (NFR-7 AC1, work plan D20). It is the handler's parent, in
// the handler's process group. On Linux it is the child subreaper of
// everything below it, so a process whose parent ends is re-parented to
// it; on macOS it scans the process tree by parent pid every procPoll and
// remembers what it saw. On SIGTERM, SIGINT or SIGHUP it sends SIGTERM to
// the processes outside the group (the group already got the signal), and
// after the grace period SIGKILL to all of them. When the handler ends it
// kills whatever is left, scanning until nothing is, and exits as the
// handler did.
func SuperviseMain(args []string, stderr io.Writer) int {
	if len(args) < 4 || args[2] != "--" {
		fmt.Fprintln(stderr, "usage: agent-loop supervise <grace-ms> <path> -- <argv...>")
		return 2
	}
	ms, err := strconv.Atoi(args[0])
	if err != nil || ms < 0 {
		fmt.Fprintf(stderr, "agent-loop supervise: bad grace %q\n", args[0])
		return 2
	}
	grace := time.Duration(ms) * time.Millisecond
	if err := setSubreaper(); err != nil {
		fmt.Fprintf(stderr, "agent-loop supervise: subreaper: %v\n", err)
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)

	p, err := os.StartProcess(args[1], args[3:], &os.ProcAttr{Files: []*os.File{os.Stdin, os.Stdout, os.Stderr}})
	if err != nil {
		fmt.Fprintf(stderr, "agent-loop supervise: %v\n", err)
		return 127
	}
	t := &tracker{self: os.Getpid(), pgid: syscall.Getpgrp(), seen: map[int]int64{}}
	exited := make(chan syscall.WaitStatus, 1)
	go reap(p.Pid, exited)

	tick := time.NewTicker(procPoll)
	defer tick.Stop()
	var kill <-chan time.Time
	var ws syscall.WaitStatus
wait:
	for {
		select {
		case ws = <-exited:
			break wait
		case <-tick.C:
			t.scan()
		case <-sigs:
			if kill == nil {
				t.signal(syscall.SIGTERM)
				kill = time.After(grace)
			}
		case <-kill:
			t.signal(syscall.SIGKILL)
		}
	}
	t.killAll()

	if ws.Signaled() {
		// End the same way, so the runner sees a signalled handler.
		signal.Reset(ws.Signal())
		syscall.Kill(os.Getpid(), ws.Signal())
		time.Sleep(time.Second)
		return 128 + int(ws.Signal())
	}
	return ws.ExitStatus()
}

// reap waits for every child: the handler, and on Linux the processes
// re-parented to the supervisor. It reports the handler's status.
func reap(handler int, exited chan<- syscall.WaitStatus) {
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, 0, nil)
		if err != nil {
			// ECHILD: none now, but an orphan may still be re-parented here.
			time.Sleep(procPoll)
			continue
		}
		if pid == handler {
			exited <- ws
		}
	}
}

// tracker remembers every process seen below the supervisor, so one that
// was re-parented away (macOS) is still stopped.
type tracker struct {
	self, pgid int
	seen       map[int]int64 // pid → start
}

// scan returns the live descendants of the supervisor and of every
// process seen before, and remembers them.
func (t *tracker) scan() []int {
	tab, err := procTable()
	if err != nil {
		return nil
	}
	children := map[int][]int{}
	for pid, p := range tab {
		children[p.ppid] = append(children[p.ppid], pid)
	}
	queue := []int{t.self}
	for pid, start := range t.seen {
		if p, ok := tab[pid]; ok && p.start == start {
			queue = append(queue, pid)
		} else {
			delete(t.seen, pid)
		}
	}
	var live []int
	visited := map[int]bool{}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if visited[pid] {
			continue
		}
		visited[pid] = true
		if pid != t.self {
			live = append(live, pid)
			t.seen[pid] = tab[pid].start
		}
		queue = append(queue, children[pid]...)
	}
	return live
}

// signal sends sig to every live process found. SIGTERM skips the
// supervisor's own group, which the runner signalled already.
func (t *tracker) signal(sig syscall.Signal) {
	for _, pid := range t.scan() {
		if sig == syscall.SIGTERM {
			if pg, err := syscall.Getpgid(pid); err == nil && pg == t.pgid {
				continue
			}
		}
		syscall.Kill(pid, sig)
	}
}

// killAll sends SIGKILL to every process found, and scans again until
// none is left (a process may fork while it is being killed).
func (t *tracker) killAll() {
	for range 400 {
		live := t.scan()
		if len(live) == 0 {
			return
		}
		for _, pid := range live {
			syscall.Kill(pid, syscall.SIGKILL)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
