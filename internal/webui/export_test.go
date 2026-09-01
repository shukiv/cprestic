package webui

import "os"

// PrepareSocketDirForTest exposes the directory rule to the test package.
func PrepareSocketDirForTest(dir string, mode os.FileMode) error {
	return prepareSocketDir(dir, mode)
}

// InFlightForTest exposes the per-account limit.
type InFlightForTest struct{ f inFlight }

func (i *InFlightForTest) Enter(key string) bool { return i.f.enter(key) }
func (i *InFlightForTest) Leave(key string)      { i.f.leave(key) }
