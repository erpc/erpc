package realworld

import (
	"encoding/binary"

	"golang.org/x/sys/unix"
)

// socketOwners is the pid that last used each TCP socket, by local and
// foreign port, from the kernel's pcb list (net.inet.tcp.pcblist_n, as
// netstat -v reads it); nil if unreadable. The list names the pid itself,
// so pids is not read.
//
// The list is a run of records, each {u32 len, u32 kind, …}, padded to 8
// bytes, after an xinpgen header; per socket an xinpcb_n (kind 0x10:
// fport, lport big-endian at 16, 18) precedes its xsocket_n (kind 0x1:
// so_last_pid at 68, structs packed to 4).
func socketOwners(_ []int) map[sockKey]int {
	buf, err := unix.SysctlRaw("net.inet.tcp.pcblist_n")
	if err != nil || len(buf) < 4 {
		return nil
	}
	le := binary.LittleEndian
	out := map[sockKey]int{}
	var k sockKey
	have := false
	for off := int(le.Uint32(buf)); off+8 <= len(buf); {
		n := int(le.Uint32(buf[off:]))
		if n < 8 || off+n > len(buf) {
			return out
		}
		rec := buf[off : off+n]
		switch le.Uint32(rec[4:]) {
		case 0x10:
			have = n >= 20
			if have {
				k = sockKey{lport: int(binary.BigEndian.Uint16(rec[18:])), fport: int(binary.BigEndian.Uint16(rec[16:]))}
			}
		case 0x1:
			if have && n >= 72 {
				out[k] = int(int32(le.Uint32(rec[68:])))
			}
			have = false
		}
		off += (n + 7) &^ 7
	}
	return out
}
