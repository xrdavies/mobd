// Package proc runs a command as the root of its own process tree, so cancelling a turn can
// kill everything the agent started: a process group on Unix, a job object on Windows.
package proc
