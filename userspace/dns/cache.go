package dns

import "goos/userspace/hostname"

type entryKind uint8

const (
	entryFree entryKind = iota
	entryPositive
	entryNXDomain
	entryNoData
	// entryFailure remembers a resolution that failed, so a client retrying
	// at once does not send the same queries again.
	entryFailure
)

// entry is one cached name. Positive, negative and failure entries share the
// table and its limit.
type entry struct {
	kind      entryKind
	result    Result
	count     uint8
	name      hostname.Name
	addresses [MaxAddresses][4]byte
	expiry    uint64
	// used is the resolver's use clock at the last store or hit.
	used uint64
}

func (r *Resolver) findEntry(name *hostname.Name) int {
	for index := range r.cache[:] {
		if r.cache[index].kind != entryFree && r.cache[index].name.Equal(name) {
			return index
		}
	}
	return -1
}

// cached answers from the cache. An expired entry is removed on the way.
func (r *Resolver) cached(name *hostname.Name, now uint64) (Answer, bool) {
	index := r.findEntry(name)
	if index < 0 {
		return Answer{}, false
	}
	item := &r.cache[index]
	if item.expiry <= now {
		r.removeEntry(index)
		return Answer{}, false
	}
	r.clock++
	item.used = r.clock
	answer := Answer{Source: SourceCache, TTL: remaining(item.expiry, now)}
	switch item.kind {
	case entryPositive:
		answer.Result = ResultOK
		answer.Count = item.count
		answer.Addresses = item.addresses
		increment(&r.positiveHits)
	case entryNXDomain:
		answer.Result = ResultNotFound
		increment(&r.negativeHits)
	case entryNoData:
		answer.Result = ResultNoData
		increment(&r.negativeHits)
	default:
		answer.Result = item.result
		increment(&r.failureHits)
	}
	return answer, true
}

// remaining is the time left until expiry in whole seconds, rounded up so a
// live entry never reports zero.
func remaining(expiry, now uint64) uint32 {
	seconds := (expiry - now + second - 1) / second
	if seconds > uint64(^uint32(0)) {
		return ^uint32(0)
	}
	return uint32(seconds)
}

// store caches an outcome for ttl seconds, replacing an older entry for the
// same name. A zero TTL caches nothing.
func (r *Resolver) store(name *hostname.Name, kind entryKind, result Result, addresses *[MaxAddresses][4]byte,
	count uint8, ttl uint32, now uint64) {
	if ttl == 0 {
		return
	}
	index := r.findEntry(name)
	if index < 0 {
		if r.cacheUsed >= int(r.policy.CacheEntries) {
			r.evict(now)
		}
		index = r.freeEntry()
		if index < 0 {
			return
		}
		r.cacheUsed++
	}
	item := &r.cache[index]
	item.kind = kind
	item.result = result
	item.count = count
	item.name = *name
	if addresses != nil {
		item.addresses = *addresses
	} else {
		item.addresses = [MaxAddresses][4]byte{}
	}
	item.expiry = addTime(now, uint64(ttl)*second)
	r.clock++
	item.used = r.clock
}

func (r *Resolver) freeEntry() int {
	for index := range r.cache[:] {
		if r.cache[index].kind == entryFree {
			return index
		}
	}
	return -1
}

func (r *Resolver) removeEntry(index int) {
	r.cache[index].kind = entryFree
	r.cache[index].name.Size = 0
	r.cacheUsed--
}

// evict removes one entry: the first expired one, or else the least recently
// used, the lower index winning a tie.
func (r *Resolver) evict(now uint64) {
	victim := -1
	for index := range r.cache[:] {
		item := &r.cache[index]
		if item.kind == entryFree {
			continue
		}
		if item.expiry <= now {
			victim = index
			break
		}
		if victim < 0 || item.used < r.cache[victim].used {
			victim = index
		}
	}
	if victim >= 0 {
		r.removeEntry(victim)
	}
}

// Flush empties the cache: positive, negative and failure entries alike.
func (r *Resolver) Flush() {
	for index := range r.cache[:] {
		r.cache[index].kind = entryFree
		r.cache[index].name.Size = 0
	}
	r.cacheUsed = 0
}
