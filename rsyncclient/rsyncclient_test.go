package rsyncclient_test

import (
	"bytes"
	"context"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gokrazy/rsync/internal/rsyncopts"
	"github.com/gokrazy/rsync/internal/rsynctest"
	"github.com/gokrazy/rsync/internal/testlogger"
	"github.com/gokrazy/rsync/rsyncclient"
	"github.com/gokrazy/rsync/rsyncd"
	"github.com/google/go-cmp/cmp"
)

func ExampleClient_Run_receiveFromSubprocess() {
	args, src, dest := []string{"-av"}, "/usr/share/man", "/tmp/man"

	// Start an rsync server and run an rsync client on its stdin/stdout.
	rsync := exec.Command("rsync",
		// TODO: derive serverOptions from args
		"--server",
		"--sender",
		"-nlogDtpr",
		".",
		src)
	stdin, err := rsync.StdinPipe()
	if err != nil {
		log.Fatal(err)
	}
	stdout, err := rsync.StdoutPipe()
	if err != nil {
		log.Fatal(err)
	}
	if err := rsync.Start(); err != nil {
		log.Fatal(err)
	}
	// Create an io.ReadWriter from a Reader and a Writer.
	rw := &struct {
		io.Reader
		io.Writer
	}{
		Reader: stdout, // The client reads from the server's stdout.
		Writer: stdin,  // The client writes to the server's stdin.
	}

	client, err := rsyncclient.New(args)
	if err != nil {
		log.Fatal(err)
	}
	if err := client.Run(context.Background(), rw, []string{dest}); err != nil {
		log.Fatal(err)
	}
}

func ExampleClient_Run_sendToGoroutine() {
	args, src, dest := []string{"-av"}, "/usr/share/man", "/tmp/man"

	// Start an rsync server and run an rsync client on
	// an io.Pipe()-backend stdin/stdout.
	rsync, err := rsyncd.NewServer(nil)
	if err != nil {
		log.Fatal(err)
	}
	// stdin from the view of the rsync server
	stdinrd, stdinwr := io.Pipe()
	stdoutrd, stdoutwr := io.Pipe()
	go func() {
		conn := rsync.NewConnection(stdinrd, stdoutwr)
		serverArgs := append([]string{"--server"}, args...)
		serverArgs = append(serverArgs, ".", dest)
		pc, err := rsyncopts.ParseArguments(serverArgs)
		if err != nil {
			log.Fatalf("parsing server args: %v", err)
		}
		opts := pc.Options
		paths := pc.RemainingArgs[1:]
		err = rsync.HandleConn(nil, conn, paths, opts, true /* negotiate */)
		if err != nil {
			log.Fatal(err)
		}
	}()

	// Create an io.ReadWriter from a Reader and a Writer.
	rw := &struct {
		io.Reader
		io.Writer
	}{
		Reader: stdoutrd, // The client reads from the server's stdout.
		Writer: stdinwr,  // The client writes to the server's stdin.
	}

	client, err := rsyncclient.New(args, rsyncclient.WithSender())
	if err != nil {
		log.Fatal(err)
	}
	if err := client.Run(context.Background(), rw, []string{src}); err != nil {
		log.Fatal(err)
	}
}

type readWriter struct {
	io.Reader
	io.Writer
}

func TestClientCommand(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	// Start an rsync process directly for the server part of the test.
	rsync := exec.Command(rsynctest.AnyRsync(t),
		"--server",
		"--sender",
		"-nlogDtpr",
		".",
		tmp)
	wc, err := rsync.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	rc, err := rsync.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	rw := &readWriter{
		Reader: rc,
		Writer: wc,
	}
	if err := rsync.Start(); err != nil {
		t.Fatal(err)
	}

	client, err := rsyncclient.New([]string{"-av"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Run(t.Context(), rw, []string{"."}); err != nil {
		t.Fatal(err)
	}
}

func TestClientServerModule(t *testing.T) {
	t.Parallel()

	stderr := testlogger.New(t)
	tmp := t.TempDir()

	src := filepath.Join(tmp, "src")
	dest := filepath.Join(tmp, "dest")
	const hello = "world"
	if err := os.MkdirAll(src, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "hello"), []byte(hello), 0644); err != nil {
		t.Fatal(err)
	}

	mod := rsyncd.Module{
		Name: "tmp",
		Path: src,
	}
	rsync, err := rsyncd.NewServer([]rsyncd.Module{mod}, rsyncd.WithStderr(stderr))
	if err != nil {
		t.Fatal(err)
	}
	const negotiate = true
	// stdin from the view of the rsync server
	stdinrd, stdinwr := io.Pipe()
	stdoutrd, stdoutwr := io.Pipe()
	conn := rsync.NewConnection(stdinrd, stdoutwr)
	args := []string{"-av"}
	serverArgs := append([]string{"--server", "--sender"}, args...)
	serverArgs = append(serverArgs, ".", "./")
	pc, err := rsyncopts.ParseArguments(serverArgs)
	if err != nil {
		t.Fatalf("parsing server args: %v", err)
	}
	t.Logf("pc.RemainingArgs=%q", pc.RemainingArgs)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := rsync.HandleConn(&mod, conn, pc.RemainingArgs[1:], pc.Options, negotiate)
		if err != nil {
			t.Error(err)
		}
	}()

	rw := &readWriter{
		Reader: stdoutrd,
		Writer: stdinwr,
	}
	client, err := rsyncclient.New(args)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Run(t.Context(), rw, []string{dest}); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(dest, "hello"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte(hello)) {
		t.Errorf("hello: unexpected contents: diff (-want +got):\n%s", cmp.Diff([]byte(hello), got))
	}

	// Ensure an error would be displayed, if any.
	wg.Wait()
}

// like TestClientServerModule, but without a module,
// i.e. using the command calling convention.
func TestClientServerCommand(t *testing.T) {
	t.Parallel()

	stderr := testlogger.New(t)
	tmp := t.TempDir()

	src := filepath.Join(tmp, "src") + "/"
	dest := filepath.Join(tmp, "dest")
	const hello = "world"
	if err := os.MkdirAll(src, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "hello"), []byte(hello), 0644); err != nil {
		t.Fatal(err)
	}

	rsync, err := rsyncd.NewServer(nil, rsyncd.WithStderr(stderr))
	if err != nil {
		t.Fatal(err)
	}
	const negotiate = true
	// stdin from the view of the rsync server
	stdinrd, stdinwr := io.Pipe()
	stdoutrd, stdoutwr := io.Pipe()
	conn := rsync.NewConnection(stdinrd, stdoutwr)
	args := []string{"-av"}
	serverArgs := append([]string{"--server", "--sender"}, args...)
	serverArgs = append(serverArgs, ".", src)
	pc, err := rsyncopts.ParseArguments(serverArgs)
	if err != nil {
		t.Fatalf("parsing server args: %v", err)
	}
	t.Logf("pc.RemainingArgs=%q", pc.RemainingArgs)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := rsync.HandleConn(nil, conn, pc.RemainingArgs[1:], pc.Options, negotiate)
		if err != nil {
			t.Error(err)
		}
	}()

	rw := &readWriter{
		Reader: stdoutrd,
		Writer: stdinwr,
	}
	client, err := rsyncclient.New(args)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Run(t.Context(), rw, []string{dest}); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(dest, "hello"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte(hello)) {
		t.Errorf("hello: unexpected contents: diff (-want +got):\n%s", cmp.Diff([]byte(hello), got))
	}

	// Ensure an error would be displayed, if any.
	wg.Wait()
}

// like TestClientServerCommand, but sending data instead of receiving.
func TestClientServerCommandSender(t *testing.T) {
	t.Parallel()

	stderr := testlogger.New(t)
	tmp := t.TempDir()

	src := filepath.Join(tmp, "src") + "/"
	dest := filepath.Join(tmp, "dest")
	const hello = "world"
	if err := os.MkdirAll(src, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "hello"), []byte(hello), 0644); err != nil {
		t.Fatal(err)
	}

	rsync, err := rsyncd.NewServer(nil, rsyncd.WithStderr(stderr))
	if err != nil {
		t.Fatal(err)
	}
	const negotiate = true
	// stdin from the view of the rsync server
	stdinrd, stdinwr := io.Pipe()
	stdoutrd, stdoutwr := io.Pipe()
	conn := rsync.NewConnection(stdinrd, stdoutwr)
	args := []string{"-av"}
	serverArgs := append([]string{"--server"}, args...)
	serverArgs = append(serverArgs, ".", dest)
	pc, err := rsyncopts.ParseArguments(serverArgs)
	if err != nil {
		t.Fatalf("parsing server args: %v", err)
	}
	t.Logf("pc.RemainingArgs=%q", pc.RemainingArgs)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := rsync.HandleConn(nil, conn, pc.RemainingArgs[1:], pc.Options, negotiate)
		if err != nil {
			t.Error(err)
		}
	}()

	rw := &readWriter{
		Reader: stdoutrd,
		Writer: stdinwr,
	}
	client, err := rsyncclient.New(args, rsyncclient.WithSender())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Run(t.Context(), rw, []string{src}); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(dest, "hello"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte(hello)) {
		t.Errorf("hello: unexpected contents: diff (-want +got):\n%s", cmp.Diff([]byte(hello), got))
	}

	// Ensure an error would be displayed, if any.
	wg.Wait()
}
