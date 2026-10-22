package realworld

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// socketOwner is the pid, among pids, that holds the TCP socket with local
// port lport and foreign port fport; 0 if none. /proc/net/tcp{,6} name the
// socket's inode, and the owner's /proc/<pid>/fd links to "socket:[inode]".
func socketOwner(lport, fport int, pids []int) int {
	inode := ""
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		if inode = socketInode(f, lport, fport); inode != "" {
			break
		}
	}
	if inode == "" {
		return 0
	}
	want := "socket:[" + inode + "]"
	for _, pid := range pids {
		dir := fmt.Sprintf("/proc/%d/fd", pid)
		fds, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			if l, err := os.Readlink(dir + "/" + fd.Name()); err == nil && l == want {
				return pid
			}
		}
	}
	return 0
}

// socketInode is the inode of the socket in table f (/proc/net/tcp
// format: "sl local rem st … inode", addresses as HEXIP:HEXPORT) with
// local port lport and remote port fport; "" if none.
func socketInode(f string, lport, fport int) string {
	fh, err := os.Open(f)
	if err != nil {
		return ""
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Scan() // header
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 10 {
			continue
		}
		if hexPort(fields[1]) == lport && hexPort(fields[2]) == fport {
			return fields[9]
		}
	}
	return ""
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
