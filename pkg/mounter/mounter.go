package mounter

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/CloudNativeAI/model-csi-driver/pkg/logger"
	"github.com/moby/sys/mountinfo"
	"github.com/pkg/errors"
)

func execCmd(ctx context.Context, command string, args ...string) (string, error) {
	logger.WithContext(ctx).Infof("exec command: %s %s", command, strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, command, args...)
	_out, err := cmd.CombinedOutput()
	out := string(_out)
	if err != nil {
		return out, err
	}
	return out, nil
}

func Mount(ctx context.Context, builder Builder) error {
	cmd, err := builder.Build()
	if err != nil {
		return err
	}
	if out, err := execCmd(ctx, cmd.command, cmd.args...); err != nil {
		return fmt.Errorf("mount failed: %v %s output %s", err, cmd, string(out))
	}
	return nil
}

func UMount(ctx context.Context, mountPoint string, lazy bool) error {
	umountCmd := "umount"
	if mountPoint == "" {
		return errors.New("target is not specified for unmounting the volume")
	}
	var out string
	var err error

	if lazy {
		out, err = execCmd(ctx, umountCmd, "--lazy", mountPoint)
	} else {
		out, err = execCmd(ctx, umountCmd, mountPoint)
	}
	if err != nil && (!strings.Contains(err.Error(), "not mounted") && !strings.Contains(err.Error(), "mountpoint not found")) {
		return fmt.Errorf("unmounting failed: %v cmd: '%s %s' output: %q",
			err, umountCmd, mountPoint, string(out))
	}
	return nil
}

func IsMounted(ctx context.Context, mountPoint string) (bool, error) {
	_, err := os.Stat(mountPoint)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}

	foundMountPoint := false
	_, err = mountinfo.GetMounts(func(i *mountinfo.Info) (skip bool, stop bool) {
		if i.Mountpoint == mountPoint {
			foundMountPoint = true
			return false, true
		}
		return true, false
	})
	if err != nil {
		return false, errors.Wrap(err, "get mount info")
	}

	return foundMountPoint, nil
}

func EnsureMountPoint(ctx context.Context, mountPoint string) error {
	_, err := os.Stat(mountPoint)
	if err == nil {
		return nil
	}
	if os.IsNotExist(err) {
		return os.MkdirAll(mountPoint, 0755)
	}
	return err
}
