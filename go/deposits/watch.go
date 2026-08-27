package deposits

import (
	"github.com/sovrn-tech/sovren-exchange-integration/go/storage"
)

// WatchSet is the parse-time view of the exchange-controlled address set
// (data model §9). Only active entries participate in classification.
type WatchSet struct {
	entries map[string]storage.WatchedAddress
	// memoRecognizer reports whether a memo is recognized under the
	// exchange's omnibus scheme (FR-016). nil ⇒ any non-empty memo is
	// recognized; an empty memo is never recognized.
	memoRecognizer func(memo string) bool
}

// NewWatchSet builds a WatchSet from watched-address rows; inactive entries
// are excluded.
func NewWatchSet(addrs []storage.WatchedAddress) WatchSet {
	m := make(map[string]storage.WatchedAddress, len(addrs))
	for _, a := range addrs {
		if a.Active {
			m[a.Address] = a
		}
	}
	return WatchSet{entries: m}
}

// WithMemoRecognizer returns a copy using fn to recognize omnibus memos.
func (w WatchSet) WithMemoRecognizer(fn func(memo string) bool) WatchSet {
	w.memoRecognizer = fn
	return w
}

// Contains reports whether addr is exchange-controlled.
func (w WatchSet) Contains(addr string) bool {
	_, ok := w.entries[addr]
	return ok
}

// Get returns the watch entry for addr.
func (w WatchSet) Get(addr string) (storage.WatchedAddress, bool) {
	e, ok := w.entries[addr]
	return e, ok
}

// Len returns the number of active watched addresses.
func (w WatchSet) Len() int { return len(w.entries) }

func (w WatchSet) memoRecognized(memo string) bool {
	if memo == "" {
		return false
	}
	if w.memoRecognizer != nil {
		return w.memoRecognizer(memo)
	}
	return true
}

// watchStats classifies an address set against the watch set.
func (w WatchSet) watchStats(addrs []string) (anyWatched, allWatched bool) {
	allWatched = len(addrs) > 0
	for _, a := range addrs {
		if w.Contains(a) {
			anyWatched = true
		} else {
			allWatched = false
		}
	}
	return anyWatched, allWatched
}
