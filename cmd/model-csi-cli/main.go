package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"

	"github.com/CloudNativeAI/model-csi-driver/pkg/client"
	"github.com/CloudNativeAI/model-csi-driver/pkg/logger"
	"github.com/CloudNativeAI/model-csi-driver/pkg/status"
)

var revision string
var buildTime string

type VolumeInfo struct {
	Addr   string
	Status status.Status
}

func getVolumeInfo(c *cli.Context) (*VolumeInfo, error) {
	workDir := c.String("workdir")
	sockPath := filepath.Join(workDir, "csi", "csi.sock")
	statusPath := filepath.Join(workDir, "status.json")
	statusBytes, err := os.ReadFile(statusPath)
	if err != nil {
		return nil, errors.Wrapf(err, "read status file: %s", statusPath)
	}
	var status status.Status
	if err := json.Unmarshal(statusBytes, &status); err != nil {
		return nil, errors.Wrapf(err, "unmarshal status file: %s", statusPath)
	}
	absSockPath, err := filepath.Abs(sockPath)
	if err != nil {
		return nil, errors.Wrapf(err, "get absolute path of sock file: %s", sockPath)
	}
	return &VolumeInfo{
		Addr:   fmt.Sprintf("unix://%s", absSockPath),
		Status: status,
	}, nil
}

func main() {
	logger.Logger().SetFormatter(&logrus.TextFormatter{
		FullTimestamp:   true,
		TimestampFormat: time.RFC3339Nano,
	})

	version := fmt.Sprintf("%s.%s", revision, buildTime)

	app := &cli.App{
		Name:    "model-csi-cli",
		Usage:   "A Kubernetes CSI driver CLI for model image",
		Version: version,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "log-level", Value: "info", Usage: "Set the logging level [trace, debug, info, warn, error, fatal, panic]"},
			&cli.StringFlag{Name: "workdir", Value: "/home/admin/model-csi", Usage: "The work directory for model csi"},
		},
		Commands: []*cli.Command{
			{
				Name:  "mount",
				Usage: "Mount a model by a specified reference and id",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "type", Required: false, Usage: "The model type to mount", Value: "image"},
					&cli.StringFlag{Name: "reference", Required: true, Usage: "The model reference to mount"},
					&cli.StringFlag{Name: "mount-id", Required: true, Usage: "The mount id"},
					&cli.BoolFlag{Name: "check-disk-quota", Required: false, Usage: "The disk quota check", Value: false},
				},
				Action: func(c *cli.Context) error {
					info, err := getVolumeInfo(c)
					if err != nil {
						return err
					}
					mountID := c.String("mount-id")

					client, err := client.NewHTTPClient(info.Addr)
					if err != nil {
						return errors.Wrap(err, "create client")
					}

					_, err = client.CreateMount(c.Context, info.Status.VolumeName, mountID, c.String("reference"), c.Bool("check-disk-quota"))
					if err != nil {
						return errors.Wrap(err, "create mount")
					}
					fmt.Println(mountID)

					return nil
				},
			},
			{
				Name:  "umount",
				Usage: "Umount a model by a specified mount id",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "mount-id", Required: true, Usage: "The mount id"},
				},
				Action: func(c *cli.Context) error {
					info, err := getVolumeInfo(c)
					if err != nil {
						return err
					}
					mountID := c.String("mount-id")

					client, err := client.NewHTTPClient(info.Addr)
					if err != nil {
						return errors.Wrap(err, "create client")
					}

					if err := client.DeleteMount(c.Context, info.Status.VolumeName, mountID); err != nil {
						return errors.Wrap(err, "delete mount")
					}
					fmt.Println(mountID)

					return nil
				},
			},
			{
				Name:  "list",
				Usage: "List all mounted models",
				Flags: []cli.Flag{},
				Action: func(c *cli.Context) error {
					info, err := getVolumeInfo(c)
					if err != nil {
						return err
					}

					client, err := client.NewHTTPClient(info.Addr)
					if err != nil {
						return errors.Wrap(err, "create client")
					}

					mounts, err := client.ListMounts(c.Context, info.Status.VolumeName)
					if err != nil {
						return errors.Wrap(err, "list mounts")
					}

					tw := tabwriter.NewWriter(os.Stdout, 1, 8, 1, '\t', 0)
					fmt.Fprintf(tw, "%s\t%s\t%s\n", "Mount ID", "Reference", "State")
					for _, mount := range mounts {
						fmt.Fprintf(tw, "%s\t%s\t%s\n", mount.MountID, mount.Reference, mount.State)
					}
					tw.Flush()

					return nil
				},
			},
		},
	}

	err := app.Run(os.Args)
	if err != nil {
		logger.Logger().Fatal(err)
	}
}
