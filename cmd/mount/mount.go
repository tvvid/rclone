// Package mount implents a FUSE mounting system for rclone remotes.

// +build linux darwin freebsd

package mount

import (
	"os"
	"os/signal"
	"syscall"

	"bazil.org/fuse"
	fusefs "bazil.org/fuse/fs"
	"github.com/ncw/rclone/cmd/mountlib"
	"github.com/ncw/rclone/fs"
	"github.com/ncw/rclone/vfs"
	"github.com/ncw/rclone/vfs/vfsflags"
	"github.com/pkg/errors"
)

func init() {
	mountlib.NewMountCommand("mount", Mount)
}

// mountOptions configures the options from the command line flags
func mountOptions(device string) (options []fuse.MountOption) {
	options = []fuse.MountOption{
		fuse.MaxReadahead(uint32(mountlib.MaxReadAhead)),
		fuse.Subtype("rclone"),
		fuse.FSName(device), fuse.VolumeName(device),
		fuse.NoAppleDouble(),
		fuse.NoAppleXattr(),

		// Options from benchmarking in the fuse module
		//fuse.MaxReadahead(64 * 1024 * 1024),
		//fuse.AsyncRead(), - FIXME this causes
		// ReadFileHandle.Read error: read /home/files/ISOs/xubuntu-15.10-desktop-amd64.iso: bad file descriptor
		// which is probably related to errors people are having
		//fuse.WritebackCache(),
	}
	if mountlib.AllowNonEmpty {
		options = append(options, fuse.AllowNonEmptyMount())
	}
	if mountlib.AllowOther {
		options = append(options, fuse.AllowOther())
	}
	if mountlib.AllowRoot {
		options = append(options, fuse.AllowRoot())
	}
	if mountlib.DefaultPermissions {
		options = append(options, fuse.DefaultPermissions())
	}
	if vfsflags.Opt.ReadOnly {
		options = append(options, fuse.ReadOnly())
	}
	if mountlib.WritebackCache {
		options = append(options, fuse.WritebackCache())
	}
	if len(mountlib.ExtraOptions) > 0 {
		fs.Errorf(nil, "-o/--option not supported with this FUSE backend")
	}
	if len(mountlib.ExtraOptions) > 0 {
		fs.Errorf(nil, "--fuse-flag not supported with this FUSE backend")
	}
	return options
}

// mount the file system
//
// The mount point will be ready when this returns.
//
// returns an error, and an error channel for the serve process to
// report an error when fusermount is called.
func mount(f fs.Fs, mountpoint string) (*vfs.VFS, <-chan error, func() error, error) {
	fs.Debugf(f, "Mounting on %q", mountpoint)
	c, err := fuse.Mount(mountpoint, mountOptions(f.Name()+":"+f.Root())...)
	if err != nil {
		return nil, nil, nil, err
	}

	filesys := NewFS(f)
	server := fusefs.New(c, nil)

	// Serve the mount point in the background returning error to errChan
	errChan := make(chan error, 1)
	go func() {
		err := server.Serve(filesys)
		closeErr := c.Close()
		if err == nil {
			err = closeErr
		}
		errChan <- err
	}()

	// check if the mount process has an error to report
	<-c.Ready
	if err := c.MountError; err != nil {
		return nil, nil, nil, err
	}

	unmount := func() error {
		return fuse.Unmount(mountpoint)
	}

	return filesys.VFS, errChan, unmount, nil
}

// Mount mounts the remote at mountpoint.
//
// If noModTime is set then it
func Mount(f fs.Fs, mountpoint string) error {
	if mountlib.DebugFUSE {
		fuse.Debug = func(msg interface{}) {
			fs.Debugf("fuse", "%v", msg)
		}
	}

	// Mount it
	FS, errChan, unmount, err := mount(f, mountpoint)
	if err != nil {
		return errors.Wrap(err, "failed to mount FUSE fs")
	}

	sigInt := make(chan os.Signal, 1)
	signal.Notify(sigInt, syscall.SIGINT, syscall.SIGTERM)
	sigHup := make(chan os.Signal, 1)
	signal.Notify(sigHup, syscall.SIGHUP)

waitloop:
	for {
		select {
		// umount triggered outside the app
		case err = <-errChan:
			break waitloop
		// Program abort: umount
		case <-sigInt:
			err = unmount()
			break waitloop
		// user sent SIGHUP to clear the cache
		case <-sigHup:
			root, err := FS.Root()
			if err != nil {
				fs.Errorf(f, "Error reading root: %v", err)
			} else {
				root.ForgetAll()
			}
		}
	}

	if err != nil {
		return errors.Wrap(err, "failed to umount FUSE fs")
	}

	return nil
}
