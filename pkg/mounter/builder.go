package mounter

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/pkg/errors"
)

type TmpfsBuilder interface {
	Tmpfs() SizeLimiter
}

type MountPointer interface {
	MountPoint(path string) Builder
}

type BindFrom interface {
	From(path string) MountPointer
}

type SizeLimiter interface {
	Size(sizeInBytes string) MountPointer
}

type Builder interface {
	Build() (MountCmd, error)
}

type MountBuilder struct {
	command    string
	targetPath string
	args       []string
}

func NewBuilder() *MountBuilder {
	return &MountBuilder{
		command: "mount",
	}
}

type MountCmd struct {
	command string
	args    []string
}

func (cmd MountCmd) String() string {
	return fmt.Sprintf("cmd: '%s %s'", cmd.command, strings.Join(cmd.args, "|"))
}

func (b *MountBuilder) Tmpfs() SizeLimiter {
	b.args = append(b.args, "-t", "tmpfs")
	return b
}

func (b *MountBuilder) Bind() BindFrom {
	b.args = append(b.args, "--bind")
	return b
}

func (b *MountBuilder) RBind() BindFrom {
	b.args = append(b.args, "--rbind")
	return b
}

func (b *MountBuilder) From(path string) MountPointer {
	b.args = append(b.args, path)
	return b
}

func (b *MountBuilder) Size(sizeInBytes string) MountPointer {
	size, _ := strconv.ParseUint(sizeInBytes, 10, 64)
	size = uint64(math.Min(2<<30, float64(size)))
	b.args = append(b.args, "-o")
	b.args = append(b.args, fmt.Sprintf("size=%s", strconv.FormatUint(size, 10)), "tmpfs")
	return b
}

func (b *MountBuilder) MountPoint(path string) Builder {
	b.targetPath = path
	b.args = append(b.args, path)
	return b
}

func (b *MountBuilder) Build() (MountCmd, error) {
	if len(b.targetPath) == 0 {
		return MountCmd{}, errors.New("mountPoint is required")
	}
	if err := os.MkdirAll(b.targetPath, 0777); err != nil {
		return MountCmd{}, fmt.Errorf("failed to make dir for targetpath %s, err: %v", b.targetPath, err)
	}
	return MountCmd{
		command: b.command,
		args:    b.args,
	}, nil
}
