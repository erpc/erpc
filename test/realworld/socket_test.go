package realworld

import (
	"sync"
	"time"
)

// sockKey is a TCP socket by local and foreign port.
type sockKey struct{ lport, fport int }

// ownerTable answers socket owners from whole reads of the OS socket table,
// one at a time per phase: every lookup waiting while a read runs takes the
// next read's answer. A read costs the size of the host's socket table, and
// a read per connection, many phases at once, kept the kernel busy for
// minutes (proxy waits of 5 min with 18 phases).
type ownerTable struct {
	mu      sync.Mutex
	done    *sync.Cond
	reading bool
	readAt  time.Time // when the last finished read started
	owners  map[sockKey]int
}

func newOwnerTable() *ownerTable {
	o := &ownerTable{}
	o.done = sync.NewCond(&o.mu)
	return o
}

// owner is the pid owning socket k in a table read that started at or
// after since; 0 if that read does not list it.
func (o *ownerTable) owner(k sockKey, since time.Time, pids func() []int) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	for {
		if !o.readAt.Before(since) {
			return o.owners[k]
		}
		if o.reading {
			o.done.Wait()
			continue
		}
		o.reading = true
		start := time.Now()
		o.mu.Unlock()
		m := socketOwners(pids())
		o.mu.Lock()
		o.owners, o.readAt, o.reading = m, start, false
		o.done.Broadcast()
	}
}
