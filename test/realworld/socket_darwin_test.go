package realworld

import (
	"encoding/binary"

	"golang.org/x/sys/unix"
)

// socketOwner is the pid that last used the TCP socket with local port
// lport and foreign port fport, from the kernel's pcb list
// (net.inet.tcp.pcblist_n, as netstat -v reads it); 0 if none. The list
// names the pid itself, so pids is not read.
//
// The list is a run of records, each {u32 len, u32 kind, …}, padded to 8
// bytes, after an xinpgen header; per socket an xinpcb_n (kind 0x10:
// fport, lport big-endian at 16, 18) precedes its xsocket_n (kind 0x1:
// so_last_pid at 68, structs packed to 4).
func socketOwner(lport, fport int, _ []int) int {
	buf, err := unix.SysctlRaw("net.inet.tcp.pcblist_n")
	if err != nil || len(buf) < 4 {
		return 0
	}
	le := binary.LittleEndian
	match := false
	for off := int(le.Uint32(buf)); off+8 <= len(buf); {
		n := int(le.Uint32(buf[off:]))
		if n < 8 || off+n > len(buf) {
			return 0
		}
		rec := buf[off : off+n]
		switch le.Uint32(rec[4:]) {
		case 0x10:
			match = n >= 20 && int(binary.BigEndian.Uint16(rec[16:])) == fport && int(binary.BigEndian.Uint16(rec[18:])) == lport
		case 0x1:
			if match && n >= 72 {
				return int(int32(le.Uint32(rec[68:])))
			}
		}
		off += (n + 7) &^ 7
	}
	return 0
}
