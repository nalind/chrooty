//go:build linux

package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/moby/sys/capability"
	"github.com/spf13/cobra"
)

var (
	toolsMountPoint                 = "/tools"
	toolsCwd                        = "/"
	chrootSubdirectory              = "/mnt/sysimage"
	copyToolsHosts                  = true
	mountChrootHosts                = true
	copyToolsResolvConf             = true
	mountChrootResolvConf           = true
	mountChrootDev, mountToolsDev   = true, true
	mountChrootDevPts               []string
	mountToolsDevPts                []string
	mountChrootProc, mountToolsProc = true, true
	uid, gid                        = uint32(0), uint32(0)
	groups                          []int64
	chrootRamfs, toolsRamfs         []string
	chrootTmpfs, toolsTmpfs         []string
	chrootMqueue, toolsMqueue       []string
	chrootSysfs, toolsSysfs         []string
	chrootMkdir                     []string
	chrootedMountPoint              string
	hostname                        string
	unshareUTS                      bool
)

func main() {
	if !slices.Contains(os.Args[1:], "--help") {
		if err := setupNamespaces(); err != nil {
			exitCode := 1
			if ee, ok := errors.AsType[*exec.ExitError](err); ok {
				if w, ok := ee.Sys().(syscall.WaitStatus); ok {
					exitCode = w.ExitStatus()
				}
			}
			fmt.Fprintln(os.Stderr, err)
			os.Exit(exitCode)
		}
	}

	mainCmd := &cobra.Command{
		Use:  "chrooty [flags] command [...]",
		Long: "Runs a specified tool in the mounted tools image with the current root filesystem mounted under it",
		RunE: func(cmd *cobra.Command, args []string) error {
			return doTheThing(cmd, args)
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	mainCmd.Flags().SetInterspersed(false)
	mainCmd.Flags().StringVar(&toolsMountPoint, "tools-mount-point", toolsMountPoint, "mountpoint for tools image")
	mainCmd.Flags().StringVar(&toolsCwd, "tools-cwd", toolsCwd, "working directory under tools chroot")
	mainCmd.Flags().StringVar(&chrootSubdirectory, "chroot-mount-point", chrootSubdirectory, "directory relative to --tools-mount-point to bind mount the current filesystem tree")
	mainCmd.Flags().BoolVar(&copyToolsHosts, "copy-tools-etc-hosts", copyToolsHosts, "copy /etc/hosts into tools-mount-point")
	mainCmd.Flags().BoolVar(&copyToolsResolvConf, "copy-tools-etc-resolv-conf", copyToolsResolvConf, "copy /etc/resolv.conf into tools-mount-point")
	mainCmd.Flags().BoolVar(&mountChrootHosts, "mount-chroot-etc-hosts", mountChrootHosts, "bind mount /etc/hosts from tools-mount-point under chroot-mount-point")
	mainCmd.Flags().BoolVar(&mountChrootResolvConf, "mount-chroot-etc-resolv-conf", mountChrootResolvConf, "bind mount /etc/resolv.conf from tools-mount-point under chroot-mount-point")
	mainCmd.Flags().BoolVar(&mountChrootDev, "mount-chroot-dev", mountChrootDev, "bind mount /dev under chroot-mount-point")
	mainCmd.Flags().StringArrayVar(&mountChrootDevPts, "mount-chroot-devpts", mountChrootDevPts, "mount a devpts fs at this location under tools-mount-point")
	mainCmd.Flags().BoolVar(&mountChrootProc, "mount-chroot-proc", mountChrootProc, "bind mount /proc under chroot-mount-point")
	mainCmd.Flags().BoolVar(&mountToolsDev, "mount-tools-dev", mountToolsDev, "bind mount /dev under tools-mount-point")
	mainCmd.Flags().StringArrayVar(&mountToolsDevPts, "mount-tools-devpts", mountToolsDevPts, "mount a devpts fs at this location under tools-mount-point")
	mainCmd.Flags().BoolVar(&mountToolsProc, "mount-tools-proc", mountToolsProc, "bind mount /proc under tools-mount-point")
	mainCmd.Flags().BoolVar(&unshareUTS, "unshare-uts", unshareUTS, "create a new UTS namespace")
	mainCmd.Flags().Uint32Var(&uid, "uid", uid, "set UID for tools command")
	mainCmd.Flags().Uint32Var(&gid, "gid", gid, "set GID for tools command")
	mainCmd.Flags().Int64SliceVar(&groups, "groups", groups, "set supplemental group GIDs for tools command")
	mainCmd.Flags().StringArrayVar(&toolsRamfs, "mount-tools-ramfs", nil, "mount a ramfs at this location under tools-mount-point")
	mainCmd.Flags().StringArrayVar(&chrootRamfs, "mount-chroot-ramfs", nil, "mount a ramfs at this location under chroot-mount-point")
	mainCmd.Flags().StringArrayVar(&toolsTmpfs, "mount-tools-tmpfs", nil, "mount a tmpfs at this location under tools-mount-point")
	mainCmd.Flags().StringArrayVar(&chrootTmpfs, "mount-chroot-tmpfs", nil, "mount a tmpfs at this location under chroot-mount-point")
	mainCmd.Flags().StringArrayVar(&chrootMkdir, "chroot-mkdir", nil, "create these directories under chroot-mount-point")
	mainCmd.Flags().StringArrayVar(&toolsMqueue, "mount-tools-mqueue", nil, "mount an mqueue fs at this location under tools-mount-point")
	mainCmd.Flags().StringArrayVar(&chrootMqueue, "mount-chroot-mqueue", nil, "mount an mqueue fs at this location under chroot-mount-point")
	mainCmd.Flags().StringArrayVar(&toolsSysfs, "mount-tools-sysfs", nil, "mount a sysfs fs at this location under tools-mount-point")
	mainCmd.Flags().StringArrayVar(&chrootSysfs, "mount-chroot-sysfs", nil, "mount a sysfs fs at this location under chroot-mount-point")
	mainCmd.Flags().StringVar(&hostname, "hostname", hostname, "hostname to set when --unshare-uts is enabled")

	exitCode := 0
	if err := mainCmd.Execute(); err != nil {
		exitCode = 1
		fmt.Fprintln(os.Stderr, err)
		if ee, ok := errors.AsType[*exec.ExitError](err); ok {
			if w, ok := ee.Sys().(syscall.WaitStatus); ok {
				exitCode = w.ExitStatus()
			}
		}
	}
	os.Exit(exitCode)
}
func doTheThing(_ *cobra.Command, args []string) error {
	// create a new mount namespace, possibly with a new UTS namespace
	unshareFlags := syscall.CLONE_NEWNS
	namespaceDescription := "mount"
	if unshareUTS {
		unshareFlags |= syscall.CLONE_NEWUTS
		namespaceDescription += "+uts"
	}
	if err := syscall.Unshare(unshareFlags); err != nil {
		return fmt.Errorf("creating new %s namespace: %w", namespaceDescription, err)
	}
	if unshareUTS && hostname != "" {
		// go ahead and set the hostname now that we're in our own UTS namespace
		if err := syscall.Sethostname([]byte(hostname)); err != nil {
			return fmt.Errorf("setting hostname %q in new UTS namespace: %w", hostname, err)
		}
	}

	// if the tools directory isn't a mountpoint, bind mount it over itself
	// so that we can pivot to it later
	toolsIsMountpoint, err := isMountPoint(toolsMountPoint)
	if err != nil {
		return fmt.Errorf("checking if %q is already a mountpoint: %w", toolsMountPoint, err)
	}
	if !toolsIsMountpoint {
		if err := syscall.Mount(toolsMountPoint, toolsMountPoint, "", syscall.MS_REC|syscall.MS_BIND, ""); err != nil {
			return fmt.Errorf("bind mounting tools image to itself: %w", err)
		}
	}

	// create the mount point for the chrooted mount of the root filesystem
	chrootedMountPoint = filepath.Join(toolsMountPoint, "/", chrootSubdirectory)
	if err := os.MkdirAll(chrootedMountPoint, 0o755); err != nil {
		return err
	}
	// now bind mount the root filesystem into the chroot directory
	if err := syscall.Mount("/", chrootedMountPoint, "", syscall.MS_REC|syscall.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind mounting rootfs to %q: %w", chrootedMountPoint, err)
	}
	// make sure the chrooted mounts are private, since we don't want to lose them later
	if err := syscall.Mount("", chrootedMountPoint, "", syscall.MS_PRIVATE|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("changing bind-mounted rootfs sharing mode to private: %w", err)
	}

	// add default API mountpoints unless they've been disabled
	if err := mountPath("/dev", mountToolsDev, mountChrootDev); err != nil {
		return err
	}
	if err := mountPath("/proc", mountToolsProc, mountChrootProc); err != nil {
		return err
	}

	// add any requested ramfs, tmpfs, mqueue filesystems, and empty directories
	for _, spec := range toolsRamfs {
		if err := mountRamfs(spec, toolsMountPoint, "tools"); err != nil {
			return err
		}
	}
	for _, spec := range chrootRamfs {
		if err := mountRamfs(spec, chrootedMountPoint, "chrooted"); err != nil {
			return err
		}
	}
	for _, spec := range toolsTmpfs {
		if err := mountTmpfs(spec, toolsMountPoint, "tools"); err != nil {
			return err
		}
	}
	for _, spec := range chrootTmpfs {
		if err := mountTmpfs(spec, chrootedMountPoint, "chrooted"); err != nil {
			return err
		}
	}
	for _, spec := range mountToolsDevPts {
		if err := mountDevPts(spec, toolsMountPoint, "tools"); err != nil {
			return err
		}
	}
	for _, spec := range mountChrootDevPts {
		if err := mountDevPts(spec, chrootedMountPoint, "chrooted"); err != nil {
			return err
		}
	}
	for _, spec := range toolsMqueue {
		if err := mountMqueue(spec, toolsMountPoint, "tools"); err != nil {
			return err
		}
	}
	for _, spec := range chrootMqueue {
		if err := mountMqueue(spec, chrootedMountPoint, "chrooted"); err != nil {
			return err
		}
	}
	for _, spec := range toolsSysfs {
		if err := mountSysfs(spec, toolsMountPoint, "tools"); err != nil {
			return err
		}
	}
	for _, spec := range chrootSysfs {
		if err := mountSysfs(spec, chrootedMountPoint, "chrooted"); err != nil {
			return err
		}
	}
	for _, spec := range chrootMkdir {
		if err := mkdir(spec, chrootedMountPoint); err != nil {
			return err
		}
	}

	// set up /etc/hosts for the tools sake, and by default, in the chroot, too
	if copyToolsHosts {
		if err := copyAndMountFile("/etc/hosts", mountChrootHosts); err != nil {
			return err
		}
	} else if mountChrootHosts {
		log.Print("--mount-chroot-etc-hosts ignored because --copy-tools-etc-hosts disabled")
	}
	// set up /etc/resolv.conf for the tools sake, and by default, in the chroot, too
	if copyToolsResolvConf {
		if err := copyAndMountFile("/etc/resolv.conf", mountChrootResolvConf); err != nil {
			return err
		}
	} else if mountChrootResolvConf {
		log.Print("--mount-chroot-etc-resolv-conf ignored because --copy-tools-etc-resolv-conf disabled")
	}

	// open descriptors for the old and new root directories so that we can use fchdir()
	oldRootFd, err := syscall.Open("/", syscall.O_DIRECTORY, 0)
	if err != nil {
		return fmt.Errorf("opening host root directory: %w", err)
	}
	defer func() {
		if err := syscall.Close(oldRootFd); err != nil {
			log.Printf("closing host root directory: %v", err)
		}
	}()
	newRootFd, err := syscall.Open(toolsMountPoint, syscall.O_DIRECTORY, 0)
	if err != nil {
		return fmt.Errorf("opening tools directory: %w", err)
	}
	defer func() {
		if err := syscall.Close(newRootFd); err != nil {
			log.Fatalf("closing tools directory: %v", err)
		}
	}()
	// change to the new root directory
	if err := syscall.Fchdir(newRootFd); err != nil {
		return fmt.Errorf("changing to tools directory: %w", err)
	}
	// this makes the current directory the root directory. not actually
	// sure what happens to the other one
	if err := syscall.PivotRoot(".", "."); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	// go back and clean up the old one
	if err := syscall.Fchdir(oldRootFd); err != nil {
		return fmt.Errorf("changing to host root directory: %w", err)
	}
	// make sure we only unmount things under this tree
	if err := syscall.Mount(".", ".", "", syscall.MS_SLAVE|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("changing to host root directory: %w", err)
	}
	// detach this (unnamed?) old directory
	if err := syscall.Unmount(".", syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("unmounting host root directory in mount namespace: %w", err)
	}
	// go back to a named root directory
	if err := syscall.Fchdir(newRootFd); err != nil {
		return fmt.Errorf("changing to container root directory at last: %w", err)
	}

	// now that we're under the tools mountpoint, actually run the command
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr, cmd.Dir = os.Stdin, os.Stdout, os.Stderr, toolsCwd
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uid, Gid: gid}
	for _, supplementalGid := range groups {
		if supplementalGid < 0 || supplementalGid > 0xfffffffe {
			return fmt.Errorf("supplementatl gid %d is out of range for uint32", supplementalGid)
		}
		cmd.SysProcAttr.Credential.Groups = append(cmd.SysProcAttr.Credential.Groups, uint32(supplementalGid))
	}
	return cmd.Run()
}

// mountPath recursively mounts the filesystem from the specified location
// to the same location under the tools directory, and optionally also under
// the chroot directory
func mountPath(path string, mountTools bool, mountChroot bool) error {
	rbind := func(path, chroot, description string) error {
		var st syscall.Stat_t
		type mode struct {
			path string
			mode os.FileMode
		}
		var modes []mode
		p := path
		for {
			if err := syscall.Stat(p, &st); err != nil {
				return &os.PathError{Op: "stat", Path: p, Err: err}
			}
			modes = append(modes, mode{path: p, mode: os.FileMode(st.Mode & 0o777)})
			next := filepath.Dir(p)
			if next == p {
				break
			}
			p = next
		}
		for _, mode := range slices.Backward(modes) {
			if err := os.MkdirAll(filepath.Join(chroot, mode.path), mode.mode); err != nil {
				return err
			}
		}
		if err := syscall.Mount(path, filepath.Join(chroot, path), "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
			return fmt.Errorf("bind mounting %q to %q directory: %w", path, description, err)
		}
		if err := syscall.Mount("", filepath.Join(chroot, path), "", syscall.MS_PRIVATE|syscall.MS_REC, ""); err != nil {
			return fmt.Errorf("changing bind mount of %q in %q directory sharing mode to private: %w", path, description, err)
		}
		return nil
	}
	var errs []error
	if mountTools {
		if err := rbind(path, toolsMountPoint, "tools"); err != nil {
			errs = append(errs, err)
		}
	}
	if mountChroot {
		if err := rbind(path, chrootedMountPoint, "chroot"); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// copyAndMountFile copies the contents of the named file from the filesystem
// to the same location under the tool image's mountpoint, and potentially bind
// mounts it into the same location under the chrooted root directory
func copyAndMountFile(path string, mount bool) error {
	if err := copyFile(path); err != nil {
		return err
	}
	if mount {
		if err := syscall.Mount(filepath.Join(toolsMountPoint, path), filepath.Join(chrootedMountPoint, path), "", syscall.MS_BIND, ""); err != nil {
			return fmt.Errorf("bind mounting %q to chroot: %w", path, err)
		}
	}
	return nil
}

// copyFile copies the contents of the named file from the filesystem to the
// same location under the tool image's mountpoint
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
	if err := os.Remove(filepath.Join(toolsMountPoint, path)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("making way for %q: %w", filepath.Join(toolsMountPoint, path), err)
	}
	f, err := os.OpenFile(filepath.Join(toolsMountPoint, path), os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_TRUNC, hostsInfo.Mode())
	if err != nil {
		return fmt.Errorf("creating a new %q: %w", filepath.Join(toolsMountPoint, path), err)
	}
	if _, err = io.Copy(f, bytes.NewReader(fileData)); err != nil {
		return fmt.Errorf("writing a new %q: %w", filepath.Join(toolsMountPoint, path), err)
	}
	return f.Close()
}

// mountApifs mounts an API filesystem, with optional mount options, at the
// specified location.
func mountApifs(spec, source, mountpoint, fs, description string) error {
	var mount struct {
		path    string
		options string
	}
	path, rest, ok := strings.Cut(spec, ",")
	mount.path = path
	if ok && rest != "" {
		for flag := range strings.SplitSeq(rest, ",") {
			if mount.options != "" {
				mount.options += ","
			}
			mount.options += flag
		}
	}
	if err := os.MkdirAll(filepath.Join(mountpoint, mount.path), 0o755); err != nil {
		return fmt.Errorf("ensuring that %s %s mountpoint %q exists: %w", mount.path, fs, description, err)
	}
	if err := syscall.Mount(source, filepath.Join(mountpoint, mount.path), fs, 0, mount.options); err != nil {
		return fmt.Errorf("mounting %s %s (options=%q) at %q: %w", description, fs, mount.options, filepath.Join(mountpoint, mount.path), err)
	}
	if err := syscall.Mount("", filepath.Join(mountpoint, mount.path), "", syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("changing %s %s %q sharing mode to private: %w", description, fs, filepath.Join(mountpoint, mount.path), err)
	}
	return nil
}

// mountRamfs mounts a ramfs under "mountpoint", describing it using
// "description" if it encounters an error.
// The spec takes the form of path[[,mountOption]...]
func mountRamfs(spec, mountpoint, description string) error {
	return mountApifs(spec, "ramfs", mountpoint, "ramfs", description)
}

// mountTmpfs mounts a tmpfs under "mountpoint", describing it using
// "description" if it encounters an error.
// The spec takes the form of path[[,mountOption]...]
func mountTmpfs(spec, mountpoint, description string) error {
	return mountApifs(spec, "tmpfs", mountpoint, "tmpfs", description)
}

// mountMqueue mounts a mqueue filesystem under "mountpoint", describing it
// using "description" if it encounters an error.
// The spec takes the form of path[,mode=755][[,mountOption]...]
func mountMqueue(spec, mountpoint, description string) error {
	return mountApifs(spec, "mqueue", mountpoint, "mqueue", description)
}

// mountSysfs mounts a sysfs under "mountpoint", describing it using
// "description" if it encounters an error.
// The spec takes the form of path[[,mountOption]...]
func mountSysfs(spec, mountpoint, description string) error {
	return mountApifs(spec, "sysfs", mountpoint, "sysfs", description)
}

// mountDevPts mounts a devpts fs under "mountpoint", describing it using
// "description" if it encounters an error.
// The spec takes the form of path[[,mountOption]...]
func mountDevPts(spec, mountpoint, description string) error {
	return mountApifs(spec, "devpts", mountpoint, "devpts", description)
}

// mkdir creates an empty directory under the mountpoint, ignoring errors
// indicating that there's already something in the specified location, whether
// it's a directory or not.  Intermediate directories are not automatically
// created.
// The spec takes the form of path[,mode=755]
func mkdir(spec, mountpoint string) error {
	subdir, rest, ok := strings.Cut(spec, ",")
	mode := uint32(0o755)
	uid, gid := uint32(0xffffffff), uint32(0xffffffff)
	if ok && rest != "" {
		for option := range strings.SplitSeq(rest, ",") {
			if modeSpec, ok := strings.CutPrefix(option, "mode="); ok {
				m, err := strconv.ParseUint(modeSpec, 8, 12)
				if err != nil {
					return fmt.Errorf("parsing mode= value %q as octal for mkdir %q: %w", modeSpec, subdir, err)
				}
				mode = uint32(m)
				continue
			}
			if uidSpec, ok := strings.CutPrefix(option, "uid="); ok {
				u, err := strconv.ParseUint(uidSpec, 10, 32)
				if err != nil {
					return fmt.Errorf("parsing uid= value %q for mkdir %q: %w", uidSpec, subdir, err)
				}
				uid = uint32(u)
				continue
			}
			if gidSpec, ok := strings.CutPrefix(option, "uid="); ok {
				u, err := strconv.ParseUint(gidSpec, 10, 32)
				if err != nil {
					return fmt.Errorf("parsing uid= value %q for mkdir %q: %w", gidSpec, subdir, err)
				}
				uid = uint32(u)
				continue
			}
			return fmt.Errorf("unrecognized option %q for mkdir %q", option, subdir)
		}
	}
	if err := syscall.Mkdir(filepath.Join(mountpoint, subdir), mode); err != nil && !errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("creating chrooted directory %q: %w", subdir, err)
	} else {
		if err := syscall.Chown(filepath.Join(mountpoint, subdir), int(uid), int(gid)); err != nil {
			return fmt.Errorf("chown()ing chrooted directory %q to %d:%d: %w", subdir, uid, gid, err)
		}
		if err := syscall.Chmod(filepath.Join(mountpoint, subdir), mode); err != nil {
			return fmt.Errorf("setting permissions on directory %q to %04o: %w", subdir, mode, err)
		}
	}
	return nil
}

// readIDMap reads either /proc/self/uid_map or /proc/self/gid_map, and returns
// a slice containing what it finds there, or an error if it can't read the
// contents.
func readIDMap(procfile string) ([]syscall.SysProcIDMap, error) {
	var mappings []syscall.SysProcIDMap
	content, err := os.ReadFile(procfile)
	if err != nil {
		return nil, fmt.Errorf("reading %q: %w", procfile, err)
	}
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		line := scanner.Text()
		fields := strings.Fields(line)
		if len(fields) < 3 {
			return nil, fmt.Errorf("reading %q: error at %q", procfile, line)
		}
		container, err := strconv.ParseUint(fields[0], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("reading %q: error at %q: parsing %q: %w", procfile, line, fields[0], err)
		}
		host, err := strconv.ParseUint(fields[1], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("reading %q: error at %q: parsing %q: %w", procfile, line, fields[1], err)
		}
		size, err := strconv.ParseUint(fields[2], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("reading %q: error at %q: parsing %q: %w", procfile, line, fields[2], err)
		}
		mappings = append(mappings, syscall.SysProcIDMap{HostID: int(host), ContainerID: int(container), Size: int(size)})
	}
	return mappings, scanner.Err()
}

// setupNamespaces returns nil if we're running as UID 0 with CAP_SYS_ADMIN,
// or tries to set things up that way if it hasn't already been trying
func setupNamespaces() error {
	if os.Getuid() != 0 {
		return errors.New("not started as UID 0")
	}
	caps, err := capability.NewPid2(0)
	if err != nil {
		log.Fatal(err)
		return fmt.Errorf("setting up to read capabilities: %w", err)
	}
	if err = caps.Load(); err != nil {
		log.Fatal(err)
		return fmt.Errorf("reading current capabilities: %w", err)
	}
	if caps.Get(capability.EFFECTIVE, capability.CAP_SYS_ADMIN) {
		// all good!
		return nil
	}
	if caps.Get(capability.EFFECTIVE, capability.CAP_SETFCAP) && os.Getenv("_CHROOTY_USERNS") == "" {
		runtime.LockOSThread()
		// try to re-exec ourselves in a new user namespace with the
		// same set of IDs mapped
		uidMap, err := readIDMap("/proc/self/uid_map")
		if err != nil {
			log.Fatalf("reading current UID mappings: %v", err)
		}
		gidMap, err := readIDMap("/proc/self/gid_map")
		if err != nil {
			log.Fatalf("reading current GID mappings: %v", err)
		}
		self, err := os.Executable()
		if err != nil {
			log.Fatalf("unable to determine current binary: %v", err)
		}
		cmd := exec.Command(self, os.Args[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		cmd.Env = append([]string{"_CHROOTY_USERNS=1"}, os.Environ()...)
		if cmd.SysProcAttr == nil {
			cmd.SysProcAttr = &syscall.SysProcAttr{}
		}
		cmd.SysProcAttr.UidMappings = slices.Clone(uidMap)
		for i := range cmd.SysProcAttr.UidMappings {
			cmd.SysProcAttr.UidMappings[i].HostID = cmd.SysProcAttr.UidMappings[i].ContainerID
		}
		cmd.SysProcAttr.GidMappings = slices.Clone(gidMap)
		for i := range cmd.SysProcAttr.GidMappings {
			cmd.SysProcAttr.GidMappings[i].HostID = cmd.SysProcAttr.GidMappings[i].ContainerID
		}
		cmd.SysProcAttr.Cloneflags = syscall.CLONE_NEWUSER
		cmd.SysProcAttr.GidMappingsEnableSetgroups = true
		if err = cmd.Run(); err != nil {
			return fmt.Errorf("re-executing %q in a new user namespace: %w", self, err)
		}
		os.Exit(0)
	}
	return errors.New("not started with CAP_SYS_ADMIN or CAP_SETFCAP")
}

// isMountPoint returns true if path and path/.. have different device numbers.
func isMountPoint(path string) (bool, error) {
	var st1, st2 syscall.Stat_t
	if err := syscall.Stat(path, &st1); err != nil {
		return false, err
	}
	if err := syscall.Stat(filepath.Join(path, ".."), &st2); err != nil {
		return false, err
	}
	return st1.Dev == st2.Dev, nil
}
