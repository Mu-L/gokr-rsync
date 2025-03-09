package maincmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gokrazy/rsync"
	"github.com/gokrazy/rsync/internal/log"
	"github.com/gokrazy/rsync/internal/receiver"
	"github.com/gokrazy/rsync/internal/rsyncopts"
	"github.com/gokrazy/rsync/internal/rsyncos"
	"github.com/gokrazy/rsync/internal/rsyncstats"
	"github.com/gokrazy/rsync/internal/rsyncwire"
	"github.com/gokrazy/rsync/internal/sender"
	"github.com/google/shlex"
)

// rsync/main.c:start_client
func rsyncMain(ctx context.Context, osenv rsyncos.Std, opts *rsyncopts.Options, sources []string, dest string) (*rsyncstats.TransferStats, error) {
	if opts.Verbose() {
		log.Printf("dest: %q, sources: %q", dest, sources)
		log.Printf("opts: %+v", opts)
	}
	// Guaranteed to be non-empty by caller of rsyncMain().
	src := sources[0]

	if opts.Verbose() {
		log.Printf("processing src=%s", src)
	}
	daemonConnection := 0 // no daemon
	host, path, port, err := checkForHostspec(src)
	if opts.Verbose() {
		log.Printf("host=%q, path=%q, port=%d, err=%v", host, path, port, err)
	}
	if err != nil {
		// source is local, check dest arg
		opts.SetSender()
		// TODO: remote_argv == "."?
		host, path, port, err = checkForHostspec(dest)
		if opts.Verbose() {
			log.Printf("host=%q, path=%q, port=%d, err=%v", host, path, port, err)
		}
		if path == "" {
			if opts.Verbose() {
				log.Printf("source and dest are both local!")
			}
			host = ""
			port = 0
			path = dest
			opts.SetLocalServer()
		} else {
			// dest is remote
			if port != 0 {
				if opts.ShellCommand() != "" {
					daemonConnection = 1 // daemon via remote shell
				} else {
					daemonConnection = -1 // daemon via socket
				}
			}
		}
	} else {
		// source is remote
		if port != 0 {
			if opts.ShellCommand() != "" {
				daemonConnection = 1 // daemon via remote shell
			} else {
				daemonConnection = -1 // daemon via socket
			}
		}
	}

	// TODO: if opts.AmSender(), verify extra source args have no hostspec
	other := dest
	if opts.Sender() {
		other = src
	}

	module := path
	if idx := strings.IndexByte(module, '/'); idx > -1 {
		module = module[:idx]
	}
	if opts.Verbose() {
		log.Printf("module=%q, path=%q, other=%q", module, path, other)
	}

	if daemonConnection < 0 {
		stats, err := socketClient(ctx, osenv, opts, host, path, port, other)
		if err != nil {
			return nil, err
		}
		return stats, nil
	}

	machine := host
	user := ""
	if idx := strings.IndexByte(machine, '@'); idx > -1 {
		user = machine[:idx]
		machine = machine[idx+1:]
	}
	rc, wc, err := doCmd(osenv, opts, machine, user, path, daemonConnection)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	defer wc.Close()
	conn := &readWriter{
		r: rc,
		w: wc,
	}
	negotiate := true
	if daemonConnection != 0 {
		done, err := startInbandExchange(osenv, opts, conn, module, path)
		if err != nil {
			return nil, err
		}
		if done {
			return nil, nil
		}
		negotiate = false // already done
	}
	stats, err := clientRun(osenv, opts, conn, []string{other}, negotiate)
	if err != nil {
		return nil, err
	}
	return stats, nil
}

// rsync/main.c:do_cmd
func doCmd(osenv rsyncos.Std, opts *rsyncopts.Options, machine, user, path string, daemonConnection int) (io.ReadCloser, io.WriteCloser, error) {
	if opts.Verbose() {
		log.Printf("doCmd(machine=%q, user=%q, path=%q, daemonConnection=%d)",
			machine, user, path, daemonConnection)
	}
	var args []string
	if !opts.LocalServer() {
		cmd := opts.ShellCommand()
		if cmd == "" {
			cmd = "ssh"
			if e := os.Getenv("RSYNC_RSH"); e != "" {
				cmd = e
			}
		}

		// We use shlex.Split(), whereas rsync implements its own shell-style-like
		// parsing. The nuances likely don’t matter to any users, and if so, users
		// might prefer shell-style parsing.
		var err error
		args, err = shlex.Split(cmd)
		if err != nil {
			return nil, nil, err
		}

		if user != "" && daemonConnection == 0 /* && !dashlset */ {
			args = append(args, "-l", user)
		}

		args = append(args, machine)

		args = append(args, "rsync") // TODO: flag
	} else {
		// NOTE: tridge rsync will fork and run child_main(), but we create a
		// new process because that is much simpler/cleaner in Go.
		args = append(args, os.Args[0])
	}

	if daemonConnection > 0 {
		args = append(args, "--server", "--daemon")
	} else {
		args = append(args, serverOptions(opts)...)
	}
	args = append(args, ".")

	if daemonConnection == 0 {
		args = append(args, path)
	}

	if opts.Verbose() {
		log.Printf("args: %q", args)
	}

	ssh := exec.Command(args[0], args[1:]...)
	wc, err := ssh.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	rc, err := ssh.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	ssh.Stderr = osenv.Stderr
	if err := ssh.Start(); err != nil {
		return nil, nil, err
	}

	go func() {
		// TODO: correctly terminate the main process when the underlying SSH
		// process exits.
		if err := ssh.Wait(); err != nil {
			log.Printf("remote shell exited: %v", err)
		}
	}()

	return rc, wc, nil
}

// rsync/main.c:client_run
func clientRun(osenv rsyncos.Std, opts *rsyncopts.Options, conn io.ReadWriter, paths []string, negotiate bool) (*rsyncstats.TransferStats, error) {
	crd := &rsyncwire.CountingReader{R: conn}
	cwr := &rsyncwire.CountingWriter{W: conn}
	c := &rsyncwire.Conn{
		Reader: crd,
		Writer: cwr,
	}

	if negotiate {
		if err := c.WriteInt32(rsync.ProtocolVersion); err != nil {
			return nil, err
		}
		remoteProtocol, err := c.ReadInt32()
		if err != nil {
			return nil, err
		}
		if opts.Verbose() {
			log.Printf("remote protocol: %d", remoteProtocol)
		}
	}

	seed, err := c.ReadInt32()
	if err != nil {
		return nil, fmt.Errorf("reading seed: %v", err)
	}

	mrd := &rsyncwire.MultiplexReader{
		Reader: conn,
	}
	// TODO: rearchitect such that our buffer can be smaller than the largest
	// rsync message size
	rd := bufio.NewReaderSize(mrd, 256*1024)
	c.Reader = rd

	if opts.Sender() {
		st := &sender.Transfer{
			Logger: log.New(osenv.Stderr),
			Opts:   opts,
			Conn:   c,
			Seed:   seed,
		}
		if opts.Verbose() {
			log.Printf("sender(paths=%q)", paths)
		}

		if len(paths) != 1 {
			// TODO: support more than one source
			return nil, fmt.Errorf("BUG: expected exactly one path, got %q", paths)
		}

		other := paths[0]
		trimPrefix := filepath.Base(filepath.Clean(other))
		if strings.HasSuffix(other, "/") {
			trimPrefix += "/"
		}
		stats, err := st.Do(crd, cwr, trimPrefix, other, []string{trimPrefix}, nil)
		if err != nil {
			return nil, err
		}
		return stats, nil
	}

	if len(paths) != 1 {
		return nil, fmt.Errorf("BUG: expected exactly one path, got %q", paths)
	}

	rt := &receiver.Transfer{
		Logger: log.New(osenv.Stderr),
		Opts: &receiver.TransferOpts{
			Verbose: opts.Verbose(),
			DryRun:  opts.DryRun(),

			DeleteMode:        opts.DeleteMode(),
			PreserveGid:       opts.PreserveGid(),
			PreserveUid:       opts.PreserveUid(),
			PreserveLinks:     opts.PreserveLinks(),
			PreservePerms:     opts.PreservePerms(),
			PreserveDevices:   opts.PreserveDevices(),
			PreserveSpecials:  opts.PreserveSpecials(),
			PreserveTimes:     opts.PreserveMTimes(),
			PreserveHardlinks: opts.PreserveHardLinks(),
		},
		Dest: paths[0],
		Env:  osenv,
		Conn: c,
		Seed: seed,
	}
	if opts.Verbose() {
		log.Printf("receiving to dest=%s", rt.Dest)
	}

	// TODO: this is different for client/server
	// client always sends exclusion list, server always receives

	// TODO: implement support for exclusion, send exclusion list here
	const exclusionListEnd = 0
	if err := c.WriteInt32(exclusionListEnd); err != nil {
		return nil, err
	}

	if opts.Verbose() { // TODO: should be DebugGTE(RECV, 1)
		log.Printf("exclusion list sent")
	}

	// receive file list
	if opts.Verbose() { // TODO: should be debug (FLOG)
		log.Printf("receiving file list")
	}
	fileList, err := rt.ReceiveFileList()
	if err != nil {
		return nil, err
	}
	if opts.Verbose() { // TODO: should be debugGTE(FLIST, 2)
		log.Printf("received %d names", len(fileList))
	}

	return rt.Do(c, fileList, false)
}

func clientMain(ctx context.Context, args []string, osenv rsyncos.Std) (*rsyncstats.TransferStats, error) {
	pc, err := rsyncopts.ParseArguments(osenv, args[1:], false)
	if err != nil {
		return nil, err
	}
	opts := pc.Options
	remaining := pc.RemainingArgs
	if len(remaining) == 0 {
		// help goes to stderr when no arguments were specified
		fmt.Fprintln(osenv.Stderr, opts.Help())
		return nil, fmt.Errorf("rsync error: syntax or usage error")
	}
	if len(remaining) == 1 {
		// Usages with just one SRC arg and no DEST arg list the source files
		// instead of copying.
		dest := ""
		sources := remaining
		return rsyncMain(ctx, osenv, opts, sources, dest)
	}
	dest := remaining[len(remaining)-1]
	sources := remaining[:len(remaining)-1]
	return rsyncMain(ctx, osenv, opts, sources, dest)
}
