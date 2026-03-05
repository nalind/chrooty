# chrooty
chrooty makes running certain commands easier during a container image build

One usage pattern we sometimes see is that you'd like to have a tool provided in an external image
operate on the current stage's root filesystem as if it were actually a chroot.  A frequent example
is using a package manager to install packages without needing to have the package manager installed
in the stage that is being built.

This can be accomplished by judiciously using the --mount=type=bind flag for RUN to mount the image
with the tool, having the RUN instruction set up a bind mount from the root directory to some
location inside of that image's mountpoint, chrooting into the image's root filesystem, and then
running the original intended command.  This basically makes all of that easier to consume.

Specifically, if during a container image build, you want to do the equivalent of running "dnf" from
the "registry.fedoraproject.org/fedora" image, passing to it the current stage's root directory as
its "--installroot" flag, you could:

RUN --mount=type=bind,from=chrooty,target=/usr/local/bin \
    --mount=type=bind,from=registry.fedoraproject.org/fedora,target=/tools,rw \
    chrooty dnf -y update --installroot=/mnt/sysimage && \
    chrooty dnf -y install golang --installroot=/mnt/sysimage && \
    chrooty dnf -y clean all --installroot=/mnt/sysimage

The chrooty binary provided in this image will:
* create a new user namespace if not running privileges
* create a new mount namespace
  - requires access to unshare()
* assume that the image providing the command you want to run (the "tools image") is
  mounted by the image builder, by default at /tools, or at another location specified using the
  optional --tools-mount-point flag
* recursively bind mount the root directory to a location (the "chroot mount point") under the tools
  image's mount point, by default /mnt/sysimage, or to another location specified using the
  --chroot-mount-point flag
  - requires that the tools image be mounted with the "rw" flag
* copy /etc/hosts and /etc/resolv.conf from the rootfs to the tools image, for the sake of commands
  which intend to use the network, unless --copy-tools-etc-hosts=false and/or
  --copy-tools-etc-resolv-conf=false are specified to disable this behavior
  - requires that the tools image be mounted with the "rw" flag
* if /etc/hosts and /etc/resolv.conf were copied, also bind mount them over the chroot mount point's
  /etc/hosts and /etc/resolv.conf as well, unless the --mount-chroot-etc-hosts=false and
  --mount-chroot-etc-resolv-conf=false flags are specified to disable this behavior
* assume subsequent arguments are a command to run after chrooting into the tools image mount point,
  and run that command

In the above example, the "dnf" command is provided by registry.fedoraproject.org/fedora, and the
"golang" package is installed into the current stage's root filesystem.
