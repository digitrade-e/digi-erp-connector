//go:build !windows

package link

import (
	"errors"
	"time"
)

// spawnDetached: the upgrade installs a Windows setup; elsewhere it is refused before anything is downloaded to run.
func spawnDetached(string, []string, string) error {
	return upgradeRefuse("upgrade_unsupported", "upgrade on the Windows server", "the upgrade runs a Windows setup")
}

func runSetupSilent(string, string, time.Duration) (int, error) {
	return -1, errors.New("the setup runs on Windows only")
}
