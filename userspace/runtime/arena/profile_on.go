//go:build !goos_userspace || goos_validation

package arena

// sizeProfile turns on the request-size histogram. Validation images and host
// tests keep it; it costs about a nanosecond, 5% of a small allocation, so
// other images leave it out.
const sizeProfile = true

// threadSizeBuckets is the histogram each thread keeps until it folds it.
const threadSizeBuckets = sizeBuckets
