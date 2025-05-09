//go:build linux || darwin

package receiver

import (
	"io/fs"
	"os"
	"os/user"
	"strconv"
	"syscall"
)

var amRoot = os.Getuid() == 0

var inGroup = func() map[uint32]bool {
	m := make(map[uint32]bool)
	u, err := user.Current()
	if err != nil {
		return m
	}
	gids, err := u.GroupIds()
	if err != nil {
		return m
	}
	for _, gidString := range gids {
		gid64, err := strconv.ParseInt(gidString, 0, 64)
		if err != nil {
			return m
		}
		m[uint32(gid64)] = true
	}
	return m
}()

func (rt *Transfer) setUid(f *File, local string, st fs.FileInfo) (fs.FileInfo, error) {
	stt := st.Sys().(*syscall.Stat_t)

	changeUid := rt.Opts.PreserveUid &&
		amRoot &&
		stt.Uid != uint32(f.Uid)

	changeGid := rt.Opts.PreserveGid &&
		(amRoot || inGroup[uint32(f.Gid)]) &&
		stt.Gid != uint32(f.Gid)

	if !changeUid && !changeGid {
		return st, nil
	}

	uid := stt.Uid
	if changeUid {
		uid = uint32(f.Uid)
	}
	gid := stt.Gid
	if changeGid {
		gid = uint32(f.Gid)
	}
	// TODO(go1.25): use os.Root.Lchown
	if err := os.Lchown(local, int(uid), int(gid)); err != nil {
		return nil, err
	}
	return rt.DestRoot.Lstat(f.Name)
}
