package client

import (
	"sync"

	"github.com/rs/zerolog/log"

	"go.mau.fi/mautrix-meta/pkg/messagix/table"
)

// FollowingCache stores the set of user IDs the account follows,
// populated from MQTT sync data (LSDeleteThenInsertIGContactInfo).
//
// The ContactId in IGContactInfo is the MQTT FBID — the same ID space
// used by SenderId in messages, so we can directly compare them.
type FollowingCache struct {
	following map[int64]bool // ContactId (MQTT FBID) → true
	mu        sync.RWMutex
	ready     bool // true after first ProcessContactInfo call
}

// NewFollowingCache creates a new empty following cache.
// Data will be populated when ProcessContactInfo is called with
// MQTT sync data from the listener.
func NewFollowingCache() *FollowingCache {
	return &FollowingCache{
		following: make(map[int64]bool),
	}
}

// IsFollowing checks whether the given userID (MQTT FBID) is in the following list.
// Returns false if the cache hasn't been populated yet (allowing all messages through).
func (fc *FollowingCache) IsFollowing(userID int64) bool {
	fc.mu.RLock()
	defer fc.mu.RUnlock()

	// If cache hasn't been populated yet, allow all (return true)
	if !fc.ready {
		return true
	}
	return fc.following[userID]
}

// ProcessContactInfo processes LSDeleteThenInsertIGContactInfo entries from
// MQTT sync data and builds the following map.
//
// IgFollowStatus values (determined empirically):
//   - TODO: Fill in after first run with debug logging
//
// For now, this logs all entries so we can determine the correct status values.
func (fc *FollowingCache) ProcessContactInfo(contacts []*table.LSDeleteThenInsertIGContactInfo) {
	if len(contacts) == 0 {
		return
	}

	newFollowing := make(map[int64]bool)
	statusCounts := make(map[int64]int) // IgFollowStatus → count

	for _, c := range contacts {
		if c == nil {
			continue
		}

		// Log every entry for discovery of what IgFollowStatus values mean
		log.Debug().
			Int64("contact_id", c.ContactId).
			Str("ig_id", c.IgId).
			Int64("ig_follow_status", c.IgFollowStatus).
			Int64("linked_fbid", c.LinkedFbid).
			Msg("IGContactInfo entry")

		statusCounts[c.IgFollowStatus]++

		// TODO: Once we determine which IgFollowStatus means "following",
		// add a filter here. For now, include ALL contacts so we can see
		// the full data set in logs. The current hypothesis is that
		// IgFollowStatus == 1 means mutual follow, == 2 means we follow them,
		// but this needs empirical verification.
		//
		// TEMPORARY: Include all contacts with non-zero IgFollowStatus
		if c.IgFollowStatus > 0 {
			newFollowing[c.ContactId] = true
		}
	}

	// Log the distribution of IgFollowStatus values
	for status, count := range statusCounts {
		log.Info().
			Int64("ig_follow_status", status).
			Int("count", count).
			Msg("IgFollowStatus distribution")
	}

	fc.mu.Lock()
	fc.following = newFollowing
	fc.ready = true
	fc.mu.Unlock()

	fc.mu.RLock()
	total := len(fc.following)
	fc.mu.RUnlock()

	log.Info().
		Int("total_contacts", len(contacts)).
		Int("following_count", total).
		Msg("following cache updated from MQTT sync data")
}

// Count returns the number of followed users in the cache (thread-safe).
func (fc *FollowingCache) Count() int {
	fc.mu.RLock()
	defer fc.mu.RUnlock()
	return len(fc.following)
}

// Ready returns whether the cache has been populated with initial sync data.
func (fc *FollowingCache) Ready() bool {
	fc.mu.RLock()
	defer fc.mu.RUnlock()
	return fc.ready
}
