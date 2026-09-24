package store

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"sync"
	"sync/atomic"
)

// Op is what a committed write did to an item.
type Op string

const (
	Added    Op = "added"
	Updated  Op = "updated"
	Closed   Op = "closed"
	Reopened Op = "reopened"
)

// Change names an item a write touched and what it did. It never carries the
// item's text: a subscriber that wants that reads the store.
type Change struct {
	ID  int64  `json:"id"`
	Op  Op     `json:"op"`
	Seq string `json:"seq"` // see Store.Seq
}

// subBuffer is how many changes a subscriber may fall behind by before new
// ones are dropped. Any change means "look again", so a dropped one costs
// nothing while the subscriber still has one to read.
const subBuffer = 16

type feed struct {
	mu   sync.Mutex
	subs map[chan Change]struct{}
	// A restart starts the count again, so the boot nonce keeps a page
	// rendered before it from matching a count after it.
	boot string
	n    atomic.Uint64
}

func bootNonce() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (f *feed) seq(n uint64) string { return f.boot + "." + strconv.FormatUint(n, 10) }

// Seq names how many writes this process has published, as "<boot>.<n>".
// Read before a page's queries, it lets the page tell later whether a change
// it hears of is one it already shows: a Seq read first can only understate
// what the queries saw.
func (s *Store) Seq() string { return s.feed.seq(s.feed.n.Load()) }

// Subscribe returns every change committed from now on, until stop is called.
// A subscriber that falls behind loses changes rather than slowing a write.
func (s *Store) Subscribe() (changes <-chan Change, stop func()) {
	ch := make(chan Change, subBuffer)
	s.feed.mu.Lock()
	if s.feed.subs == nil {
		s.feed.subs = map[chan Change]struct{}{}
	}
	s.feed.subs[ch] = struct{}{}
	s.feed.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			s.feed.mu.Lock()
			delete(s.feed.subs, ch)
			s.feed.mu.Unlock()
		})
	}
}

// Subscribers lets a test see that a closed stream let its subscription go.
func (s *Store) Subscribers() int {
	s.feed.mu.Lock()
	defer s.feed.mu.Unlock()
	return len(s.feed.subs)
}

func (f *feed) publish(changes []Change) {
	if len(changes) == 0 {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	seq := f.seq(f.n.Add(1))
	for ch := range f.subs {
		for _, c := range changes {
			c.Seq = seq
			select {
			case ch <- c:
			default:
			}
		}
	}
}
