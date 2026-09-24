package store

import "sync"

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
	ID int64 `json:"id"`
	Op Op    `json:"op"`
}

// subBuffer is how many changes a subscriber may fall behind by before new
// ones are dropped. Any change means "look again", so a dropped one costs
// nothing while the subscriber still has one to read.
const subBuffer = 16

type feed struct {
	mu   sync.Mutex
	subs map[chan Change]struct{}
}

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
	for ch := range f.subs {
		for _, c := range changes {
			select {
			case ch <- c:
			default:
			}
		}
	}
}
