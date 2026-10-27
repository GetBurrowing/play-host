//go:build windows

package launch

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

type jobProc struct {
	mu      sync.Mutex
	job     windows.Handle // 0 once killed: a closed handle value can be reused by the OS
	process windows.Handle
	pid     int
}

// Start creates the process suspended inside a new Job Object.
func Start(s Spec) (Proc, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("CreateJobObject: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("SetInformationJobObject: %w", err)
	}

	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), InheritHandle: 1}
	var out windows.Handle
	logPath := s.LogPath
	if logPath == "" {
		logPath = "NUL"
	}
	p16, _ := windows.UTF16PtrFromString(logPath)
	out, err = windows.CreateFile(p16, windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, sa, windows.CREATE_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("open log %s: %w", logPath, err)
	}
	defer windows.CloseHandle(out)
	nul16, _ := windows.UTF16PtrFromString("NUL")
	in, err := windows.CreateFile(nul16, windows.GENERIC_READ, windows.FILE_SHARE_READ, sa, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	defer windows.CloseHandle(in)

	// Inherit ONLY the two std handles, never the host's pipes to ffmpeg.
	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	defer attrs.Delete()
	handles := []windows.Handle{in, out}
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&handles[0]), uintptr(len(handles))*unsafe.Sizeof(handles[0])); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	si := &windows.StartupInfoEx{ProcThreadAttributeList: attrs.List()}
	si.Cb = uint32(unsafe.Sizeof(*si))
	si.Flags = windows.STARTF_USESTDHANDLES
	si.StdInput, si.StdOutput, si.StdErr = in, out, out

	cmd := windows.ComposeCommandLine(append([]string{s.Path}, s.Args...))
	cmd16, _ := windows.UTF16PtrFromString(cmd)
	app16, _ := windows.UTF16PtrFromString(s.Path)
	var dir16 *uint16
	if s.Dir != "" {
		dir16, _ = windows.UTF16PtrFromString(s.Dir)
	}
	var env *uint16
	if len(s.Env) > 0 {
		env = envBlock(append(os.Environ(), s.Env...))
	}
	var pi windows.ProcessInformation
	flags := uint32(windows.CREATE_SUSPENDED | windows.CREATE_UNICODE_ENVIRONMENT | windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_NO_WINDOW)
	if err := windows.CreateProcess(app16, cmd16, nil, nil, true, flags, env, dir16, &si.StartupInfo, &pi); err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("CreateProcess %s: %w", s.Path, err)
	}
	defer windows.CloseHandle(pi.Thread)
	if err := windows.AssignProcessToJobObject(job, pi.Process); err != nil {
		windows.TerminateProcess(pi.Process, 1)
		windows.CloseHandle(pi.Process)
		windows.CloseHandle(job)
		return nil, fmt.Errorf("AssignProcessToJobObject: %w", err)
	}
	if _, err := windows.ResumeThread(pi.Thread); err != nil {
		windows.TerminateJobObject(job, 1)
		windows.CloseHandle(pi.Process)
		windows.CloseHandle(job)
		return nil, fmt.Errorf("ResumeThread: %w", err)
	}
	return &jobProc{job: job, process: pi.Process, pid: int(pi.ProcessId)}, nil
}

func (p *jobProc) Pid() int { return p.pid }

func (p *jobProc) Wait() (int, error) {
	if _, err := windows.WaitForSingleObject(p.process, windows.INFINITE); err != nil {
		return -1, err
	}
	var code uint32
	if err := windows.GetExitCodeProcess(p.process, &code); err != nil {
		return -1, err
	}
	return int(code), nil
}

// Kill terminates every process in the job, then closes the job. It is
// idempotent: a second call does nothing, so it can never act on a handle
// value the OS has since reused for something else.
func (p *jobProc) Kill() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.job == 0 {
		return nil
	}
	err := windows.TerminateJobObject(p.job, 1)
	windows.CloseHandle(p.job)
	p.job = 0
	return err
}

func (p *jobProc) Pids() ([]int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.job == 0 {
		return nil, nil
	}
	// JOBOBJECT_BASIC_PROCESS_ID_LIST: two DWORDs then ULONG_PTR ids.
	buf := make([]uintptr, 2+256)
	if err := windows.QueryInformationJobObject(p.job, windows.JobObjectBasicProcessIdList,
		uintptr(unsafe.Pointer(&buf[0])), uint32(len(buf))*uint32(unsafe.Sizeof(buf[0])), nil); err != nil {
		return nil, err
	}
	// amd64 only: the two DWORD counters share the first ULONG_PTR slot;
	// the high half is NumberOfProcessIdsInList.
	n := int(uint32(buf[0] >> 32))
	out := make([]int, 0, n)
	for i := 0; i < n && 1+i < len(buf); i++ {
		out = append(out, int(buf[1+i]))
	}
	return out, nil
}

// envBlock is a CreateProcess environment block: NUL-separated entries, an
// extra NUL at the end, UTF-16. Entries with an interior NUL are skipped.
func envBlock(env []string) *uint16 {
	var u []uint16
	for _, e := range env {
		if e == "" || strings.ContainsRune(e, 0) {
			continue
		}
		u = append(u, utf16.Encode([]rune(e))...)
		u = append(u, 0)
	}
	u = append(u, 0)
	return &u[0]
}
