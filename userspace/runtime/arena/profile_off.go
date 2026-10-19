//go:build goos_userspace && !goos_validation

package arena

// sizeProfile turns off the request-size histogram outside validation images.
const sizeProfile = false
