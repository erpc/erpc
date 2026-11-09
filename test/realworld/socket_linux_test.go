package realworld

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// socketOwners is the pid, among pids, that holds each TCP socket, by local
// and foreign port. /proc/net/tcp{,6} name each socket's inode, and the
// owner's /proc/<pid>/fd links to "socket:[inode]".
func socketOwners(pids []int) map[sockKey]int {
	byInode := map[string]int{}
	for _, pid := range pids {
		dir := fmt.Sprintf("/proc/%d/fd", pid)
		fds, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			if l, err := os.Readlink(dir + "/" + fd.Name()); err == nil {
				if inode, ok := strings.CutPrefix(l, "socket:["); ok {
					byInode[strings.TrimSuffix(inode, "]")] = pid
				}
			}
		}
	}
	out := map[sockKey]int{}
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		readSockets(f, func(k sockKey, inode string) {
			if pid, ok := byInode[inode]; ok {
				out[k] = pid
			}
		})
	}
	return out
}

// readSockets calls fn for each socket in table f (/proc/net/tcp format:
// "sl local rem st … inode", addresses as HEXIP:HEXPORT).
func readSockets(f string, fn func(sockKey, string)) {
	fh, err := os.Open(f)
	if err != nil {
		return
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Scan() // header
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 10 {
			continue
		}
		fn(sockKey{lport: hexPort(fields[1]), fport: hexPort(fields[2])}, fields[9])
	}
}

func hexPort(addr string) int {
	i := strings.LastIndexByte(addr, ':')
	if i < 0 {
		return -1
	}
	p, err := strconv.ParseUint(addr[i+1:], 16, 16)
	if err != nil {
		return -1
	}
	return int(p)
}
