package proc

import "os"

// Spec describes a long-lived process: a desktop's X server, bus or window manager, or an app.
type Spec struct {
	Path           string
	Args, Env      []string // Args without argv[0]
	Dir            string
	Stdout, Stderr *os.File // log files; never pipes, desktop processes outlive mobd
	NewSession     bool     // setsid: the process leaves mobd's session and process group
	CgroupDir      *os.File // start inside this cgroup (CLONE_INTO_CGROUP); Linux only
	OOMScoreAdj    int      // written to /proc/<pid>/oom_score_adj after start; 0 leaves it
}

// Spawner starts processes; tests use a fake.
type Spawner interface {
	// Start returns once the process runs; wait reaps it and is called exactly once.
	Start(Spec) (pid int, wait func() error, err error)
}
