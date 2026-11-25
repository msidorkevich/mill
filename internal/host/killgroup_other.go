//go:build unix && !darwin

package host

// Outside macOS a group of zombies can still be signalled, so EPERM is always an error.
var groupLiveMembers func(pgid int) ([]int, error)
