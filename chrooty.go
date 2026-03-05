//go:build linux

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	rspec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"go.podman.io/storage/pkg/reexec"
	"go.podman.io/storage/pkg/unshare"
)

var (
	toolMountPoint        = "/tools"
	toolCwd               = "/"
	chrootMountPoint      = "/mnt/sysimage"
	copyToolsHosts        = true
	copyToolsResolvConf   = true
	mountChrootHosts      = true
	mountChrootResolvConf = true
	chrootedMountPoint    = ""
)

func main() {
	if reexec.Init() {
		return
	}

	mainCmd := &cobra.Command{
		Use:  "chrooted [flags] command [...]",
		Long: "Runs a specified command in a mounted tools image with the root filesystem mounted under it",
		RunE: func(cmd *cobra.Command, args []string) error {
			setupNamespaces()
			return doTheThing(cmd, args)
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	mainCmd.Flags().SetInterspersed(false)
	mainCmd.Flags().StringVar(&toolMountPoint, "tools-mount-point", toolMountPoint, "mountpoint for tools image")
	mainCmd.Flags().StringVar(&toolCwd, "tools-cwd", toolCwd, "working directory in tools chroot")
	mainCmd.Flags().StringVar(&chrootMountPoint, "chroot-mount-point", chrootMountPoint, "mountpoint relative to --tools-mount-point to mount the root directory")
	mainCmd.Flags().BoolVar(&copyToolsHosts, "copy-tools-etc-hosts", copyToolsHosts, "copy /etc/hosts into tools-mount-point")
	mainCmd.Flags().BoolVar(&copyToolsResolvConf, "copy-tools-etc-resolv-conf", copyToolsResolvConf, "copy /etc/resolv.conf into tools-mount-point")
	mainCmd.Flags().BoolVar(&mountChrootHosts, "mount-chroot-etc-hosts", mountChrootHosts, "bind mount /etc/hosts under chroot-mount-point")
	mainCmd.Flags().BoolVar(&mountChrootResolvConf, "mount-chroot-etc-resolv-conf", mountChrootResolvConf, "bind mount /etc/resolv.conf under chroot-mount-point")

	exitCode := 0
	err := mainCmd.Execute()
	if err != nil {
		if logrus.IsLevelEnabled(logrus.TraceLevel) {
			fmt.Fprintf(os.Stderr, "Error: %+v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if w, ok := ee.Sys().(syscall.WaitStatus); ok {
				exitCode = w.ExitStatus()
			}
		}
	}
	os.Exit(exitCode)
}

func setupNamespaces() {
	ok, err := unshare.HasCapSysAdmin()
	if err != nil {
		logrus.Fatal(err)
	}
	if !ok && os.Getenv("_CHROOTY_USERNS") == "" {
		uidMap, gidMap, err := unshare.GetHostIDMappings("")
		if err != nil {
			logrus.Fatalf("reading current ID mappings: %v", err)
		}
		cmd := unshare.Command(append([]string{reexec.Self()}, os.Args[1:]...)...)
		for _, u := range uidMap {
			cmd.UidMappings = append(cmd.UidMappings, rspec.LinuxIDMapping{
				HostID:      u.ContainerID,
				ContainerID: u.ContainerID,
				Size:        u.Size,
			})
		}
		for _, g := range gidMap {
			cmd.GidMappings = append(cmd.GidMappings, rspec.LinuxIDMapping{
				HostID:      g.ContainerID,
				ContainerID: g.ContainerID,
				Size:        g.Size,
			})
		}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		cmd.Env = append([]string{"_CHROOTY_USERNS=1"}, os.Environ()...)
		cmd.UnshareFlags = syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS
		cmd.GidMappingsEnableSetgroups = true
		unshare.ExecRunnable(cmd, nil)
		return
	}
}

func doTheThing(_ *cobra.Command, args []string) error {
	if err := syscall.Unshare(syscall.CLONE_NEWNS); err != nil {
		return fmt.Errorf("creating new mount namespace: %w", err)
	}
	chrootedMountPoint = filepath.Join(toolMountPoint, "/", chrootMountPoint)
	if err := os.MkdirAll(chrootedMountPoint, 0o700); err != nil {
		return err
	}
	if err := syscall.Mount("/", chrootedMountPoint, "bind", syscall.MS_REC|syscall.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind mounting rootfs to %q: %w", chrootedMountPoint, err)
	}
	if copyToolsHosts {
		if err := copyAndMountFile("/etc/hosts", mountChrootHosts); err != nil {
			return err
		}
	} else if mountChrootHosts {
		logrus.Warn("--mount-chroot-etc-hosts ignored because --copy-tools-etc-hosts disabled")
	}
	if copyToolsResolvConf {
		if err := copyAndMountFile("/etc/resolv.conf", mountChrootResolvConf); err != nil {
			return err
		}
	} else if mountChrootResolvConf {
		logrus.Warn("--mount-chroot-etc-resolv-conf ignored because --copy-tools-etc-resolv-conf disabled")
	}
	if err := os.Chdir(filepath.Join(toolMountPoint, toolCwd)); err != nil {
		return err
	}
	cmd := exec.Command(args[0], args[1:]...)
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Chroot = toolMountPoint
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func copyAndMountFile(path string, mount bool) error {
	if err := copyFile(path); err != nil {
		return err
	}
	if mount {
		if err := syscall.Mount(filepath.Join(toolMountPoint, path), filepath.Join(chrootedMountPoint, path), "bind", syscall.MS_BIND, ""); err != nil {
			return fmt.Errorf("bind mounting %q to chroot: %w", path, err)
		}
	}
	return nil
}

func copyFile(path string) error {
	fileData, err := os.ReadFile(path)
	if err != nil && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	hostsInfo, err := os.Stat(path)
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(toolMountPoint, path)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("making way for %q: %w", filepath.Join(toolMountPoint, path), err)
	}
	f, err := os.OpenFile(filepath.Join(toolMountPoint, path), os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_TRUNC, hostsInfo.Mode())
	if err != nil {
		return fmt.Errorf("creating a new %q: %w", filepath.Join(toolMountPoint, path), err)
	}
	if _, err = io.Copy(f, bytes.NewReader(fileData)); err != nil {
		return fmt.Errorf("writing a new %q: %w", filepath.Join(toolMountPoint, path), err)
	}
	return f.Close()
}
