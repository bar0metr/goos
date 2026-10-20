package main

// dispatchSerialBatch retains the legacy control-first admission order.
// before applies owner-side filesystem failure state before every action,
// not just once for the batch: an earlier action can quarantine shared cache.
func dispatchSerialBatch(mask uint64, count int, before, control func(), session func(int)) {
	if mask&1 != 0 {
		before()
		control()
	}
	for interest := 1; interest < count; interest++ {
		if mask&(uint64(1)<<interest) != 0 {
			before()
			session(interest)
		}
	}
}
