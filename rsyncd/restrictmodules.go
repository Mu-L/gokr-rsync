package rsyncd

import (
	"fmt"
	"os"

	"github.com/gokrazy/rsync/internal/restrict"
)

func restrictToModules(modules []Module) error {
	var roDirs, rwDirs []string
	for _, mod := range modules {
		if mod.Writable {
			if err := os.MkdirAll(mod.Path, 0755); err != nil {
				return fmt.Errorf("MkdirAll(mod=%s): %v", mod.Name, err)
			}
			rwDirs = append(rwDirs, mod.Path)
		} else {
			roDirs = append(roDirs, mod.Path)
		}
	}
	return restrict.MaybeFileSystem(roDirs, rwDirs)
}
