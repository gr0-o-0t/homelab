package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// LateMount reports whether path lives on a filesystem that is mounted at
// login rather than at boot: ecryptfs, or any mount whose mount point is the
// home directory itself (systemd-homed, pam_mount). fstype names what was
// found. Reads /proc/self/mountinfo; false when that cannot be read.
func LateMount(path, home string) (fstype string, late bool) {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return "", false
	}
	return lateMount(string(data), path, home)
}

// lateMount is LateMount on given mountinfo content.
func lateMount(mountinfo, path, home string) (string, bool) {
	path = filepath.Clean(path)
	best, bestType := "", ""
	for _, line := range strings.Split(mountinfo, "\n") {
		pre, post, ok := strings.Cut(line, " - ")
		if !ok {
			continue
		}
		f := strings.Fields(pre)
		pf := strings.Fields(post)
		if len(f) < 5 || len(pf) < 1 {
			continue
		}
		mp := unescapeMount(f[4])
		if !under(path, mp) || len(mp) < len(best) {
			continue
		}
		best, bestType = mp, pf[0]
	}
	if best == "" {
		return "", false
	}
	if bestType == "ecryptfs" {
		return bestType, true
	}
	if home != "" && best == filepath.Clean(home) {
		return bestType, true
	}
	return bestType, false
}

func under(path, dir string) bool {
	if dir == "/" {
		return true
	}
	return path == dir || strings.HasPrefix(path, dir+"/")
}

// unescapeMount undoes mountinfo's octal escapes (\040 for a space, …).
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// clockTicks is USER_HZ, which is 100 on every Linux architecture Go targets.
const clockTicks = 100

// ProcessStart returns when a process started, from /proc/<pid>/stat and the
// boot time in /proc/stat.
func ProcessStart(pid int) (time.Time, error) {
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return time.Time{}, err
	}
	ticks, err := statStartTicks(string(stat))
	if err != nil {
		return time.Time{}, err
	}
	procStat, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, err
	}
	btime, err := bootTime(string(procStat))
	if err != nil {
		return time.Time{}, err
	}
	return btime.Add(time.Duration(ticks) * time.Second / clockTicks), nil
}

// ProcessName is /proc/<pid>/comm.
func ProcessName(pid int) string {
	b, _ := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	return strings.TrimSpace(string(b))
}

// statStartTicks extracts field 22 (starttime) of /proc/<pid>/stat. The comm
// field may hold spaces and parentheses, so counting starts after the last ')'.
func statStartTicks(stat string) (uint64, error) {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, fmt.Errorf("malformed stat")
	}
	f := strings.Fields(stat[i+1:])
	// f[0] is field 3 (state), so field 22 is f[19].
	if len(f) < 20 {
		return 0, fmt.Errorf("malformed stat: %d fields", len(f))
	}
	return strconv.ParseUint(f[19], 10, 64)
}

// bootTime reads the btime line of /proc/stat.
func bootTime(procStat string) (time.Time, error) {
	for _, line := range strings.Split(procStat, "\n") {
		if v, ok := strings.CutPrefix(line, "btime "); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return time.Time{}, err
			}
			return time.Unix(n, 0), nil
		}
	}
	return time.Time{}, fmt.Errorf("no btime in /proc/stat")
}

// SessionCutoff is when this login session began, as far as the containers
// are concerned: anything started before it may hold pre-login bind mounts.
// Under systemd the parent is the user manager, started at login, so the
// cutoff survives the supervisor itself being restarted (Restart=on-failure)
// without restarting the containers it brought up. readySince — when the
// config dir became readable after waiting for it — wins when later: with
// lingering the user manager starts at boot, long before home is mounted.
func SessionCutoff(readySince time.Time) time.Time {
	cutoff := time.Now()
	if ppid := os.Getppid(); ProcessName(ppid) == "systemd" {
		if t, err := ProcessStart(ppid); err == nil {
			cutoff = t
		}
	} else if t, err := ProcessStart(os.Getpid()); err == nil {
		cutoff = t
	}
	if readySince.After(cutoff) {
		cutoff = readySince
	}
	return cutoff
}
