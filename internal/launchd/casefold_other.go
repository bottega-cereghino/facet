//go:build !darwin

package launchd

// foldsCase is false off macOS: the volumes facet's other CI job uses keep
// case, and there is no launchd there to mis-compare.
func foldsCase(string) bool { return false }
