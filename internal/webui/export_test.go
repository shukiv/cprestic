package webui

import (
	"os"

	"github.com/shuki/cprest/internal/granular"
)

// PrepareSocketDirForTest exposes the directory rule to the test package.
func PrepareSocketDirForTest(dir string, mode os.FileMode) error {
	return prepareSocketDir(dir, mode)
}

// InFlightForTest exposes the per-account limit.
type InFlightForTest struct{ f inFlight }

func (i *InFlightForTest) Enter(key string) bool { return i.f.enter(key) }
func (i *InFlightForTest) Leave(key string)      { i.f.leave(key) }

// IsUserKindForTest exposes what an account is allowed to ask for.
func IsUserKindForTest(kind string) bool { return isUserKind(granular.Kind(kind)) }

// IsSuspendedForTest exposes the suspension check, pointed at directories
// the test controls.
func IsSuspendedForTest(usersDir, suspendedDir, name string) bool {
	previousUsers, previousSuspended := cpanelUsersDir, cpanelSuspendedDir
	cpanelUsersDir, cpanelSuspendedDir = usersDir, suspendedDir
	defer func() { cpanelUsersDir, cpanelSuspendedDir = previousUsers, previousSuspended }()
	return isSuspended(name)
}
