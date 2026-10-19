//go:build windows

package main

import (
	"fmt"
	"golang.org/x/sys/windows"
	"os"
	"sync"
	"sync/atomic"
	"unsafe"
)

const childMemoryBytes = 96 << 20
const jobMessageProcessMemoryLimit = 9

// Job completions carry a numeric PID in lpOverlapped, not an OVERLAPPED
// pointer. Keep it as uintptr so the Go collector never interprets a PID as
// a pointer into the managed heap.
var getJobCompletion = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetQueuedCompletionStatus")

type jobCompletionPort struct {
	Key  uintptr
	Port windows.Handle
}
type windowsChildIsolation struct {
	job, port, process windows.Handle
	pid                uintptr
	done               chan struct{}
	closeOnce          sync.Once
	memoryExceeded     atomic.Bool
}

// Attach before sending stdin, so generated Lua cannot run before admission.
// A hard commit limit rejects allocations; the host also consumes the OS
// notification and ends this child, instead of waiting through allocator/GC
// retries until an unrelated caller deadline.
func isolateChild(process *os.Process) (childIsolation, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_PROCESS_MEMORY | windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	info.ProcessMemoryLimit = childMemoryBytes
	if _, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	port, err := windows.CreateIoCompletionPort(windows.InvalidHandle, 0, 0, 1)
	if err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	handle, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(process.Pid))
	if err != nil {
		windows.CloseHandle(port)
		windows.CloseHandle(job)
		return nil, err
	}
	association := jobCompletionPort{Key: 1, Port: port}
	if _, err = windows.SetInformationJobObject(job, windows.JobObjectAssociateCompletionPortInformation, uintptr(unsafe.Pointer(&association)), uint32(unsafe.Sizeof(association))); err != nil {
		windows.CloseHandle(handle)
		windows.CloseHandle(port)
		windows.CloseHandle(job)
		return nil, err
	}
	if err = windows.AssignProcessToJobObject(job, handle); err != nil {
		windows.CloseHandle(handle)
		windows.CloseHandle(port)
		windows.CloseHandle(job)
		return nil, err
	}
	guard := &windowsChildIsolation{job: job, port: port, process: handle, pid: uintptr(process.Pid), done: make(chan struct{})}
	go guard.watch()
	return guard, nil
}
func (g *windowsChildIsolation) watch() {
	defer close(g.done)
	for {
		var message uint32
		var key, pid uintptr
		ok, _, _ := getJobCompletion.Call(uintptr(g.port), uintptr(unsafe.Pointer(&message)), uintptr(unsafe.Pointer(&key)), uintptr(unsafe.Pointer(&pid)), uintptr(windows.INFINITE))
		if ok == 0 || key == 0 {
			return
		}
		if message == jobMessageProcessMemoryLimit && pid == g.pid {
			g.memoryExceeded.Store(true)
			// Use the handle held since admission, never reopen a possibly reused PID.
			_ = windows.TerminateProcess(g.process, uint32(windows.ERROR_NOT_ENOUGH_MEMORY))
		}
	}
}
func (g *windowsChildIsolation) Close() {
	g.closeOnce.Do(func() {
		_ = windows.CloseHandle(g.job)
		// Drain already queued resource notifications before the stop marker.
		err := windows.PostQueuedCompletionStatus(g.port, 0, 0, nil)
		if err != nil {
			_ = windows.CloseHandle(g.port)
		}
		<-g.done
		if err == nil {
			_ = windows.CloseHandle(g.port)
		}
		_ = windows.CloseHandle(g.process)
	})
}
func (g *windowsChildIsolation) Err() error {
	if g.memoryExceeded.Load() {
		return fmt.Errorf("%w (%d bytes)", errChildMemoryBudget, childMemoryBytes)
	}
	return nil
}
