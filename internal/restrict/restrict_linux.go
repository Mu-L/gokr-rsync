// Package restrict can be used to restrict further file system access of the
// process if the operating system provides an API for that.
package restrict

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/landlock-lsm/go-landlock/landlock"
)

// ExtraHook is set when testing to make the landlock rule set more permissive.
var ExtraHook func() []landlock.Rule

// As of Go 1.24, the net package Go resolver reads
// the following DNS configurations files:
var dnsLookup = []string{
	"/etc/resolv.conf",
	"/etc/hosts",
	"/etc/services",
	"/etc/nsswitch.conf",
}

var userLookup = []string{
	"/etc/passwd", // user lookup
	"/etc/group",  // group lookup
}

// ssh(1) needs to read its config and key files
var sshConfigDirs = []string{
	filepath.Join(os.Getenv("HOME"), ".ssh"), // user
	"/etc/ssh",                               // system-wide
}
var sshDirs = []string{
	"/usr", // for running ssh(1)
}
var sshDevices = []string{
	"/dev/null",
}

func MaybeFileSystem(roDirs []string, rwDirs []string) error {
	re := ExtraHook
	if re == nil {
		re = func() []landlock.Rule {
			return nil
		}
	}
	log.Printf("setting up landlock ACL (paths ro: %d, paths rw: %d)", len(roDirs), len(rwDirs))
	err := landlock.V3.BestEffort().RestrictPaths(
		append(re(), []landlock.Rule{
			landlock.ROFiles(dnsLookup...).IgnoreIfMissing(),
			landlock.ROFiles(userLookup...).IgnoreIfMissing(),
			landlock.RODirs(sshConfigDirs...).IgnoreIfMissing(),
			landlock.RODirs(sshDirs...).IgnoreIfMissing(),
			landlock.RWFiles(sshDevices...).IgnoreIfMissing(),
			landlock.RODirs(roDirs...).IgnoreIfMissing(),
			landlock.RWDirs(rwDirs...).WithRefer(),
		}...)...)
	if err != nil {
		return fmt.Errorf("landlock: %v", err)
	}
	return nil
}
