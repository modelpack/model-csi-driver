package main

import (
	"fmt"
	"os"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"

	"github.com/modelpack/model-csi-driver/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/logger"
	"github.com/modelpack/model-csi-driver/pkg/server"
)

var revision string
var buildTime string

func main() {
	logger.Logger().SetFormatter(&logrus.TextFormatter{
		FullTimestamp:   true,
		TimestampFormat: time.RFC3339Nano,
	})

	version := fmt.Sprintf("%s.%s", revision, buildTime)

	app := &cli.App{
		Name:    "model-csi-driver",
		Usage:   "A Kubernetes CSI driver for model image serving",
		Version: version,
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "log-level", Value: "info", Usage: "Set the logging level [trace, debug, info, warn, error, fatal, panic]"},
			&cli.StringFlag{
				Name:     "config",
				Usage:    "Path to configuration file",
				Required: true,
			},
		},
		Action: func(c *cli.Context) error {
			cfg, err := config.FromFile(c.String("config"))
			if err != nil {
				return errors.Wrap(err, "load config")
			}
			server, err := server.NewServer(cfg)
			if err != nil {
				return errors.Wrap(err, "create server")
			}
			if err := server.Run(c.Context); err != nil {
				return errors.Wrap(err, "run csi server")
			}
			return nil
		},
	}

	err := app.Run(os.Args)
	if err != nil {
		logger.Logger().Fatal(err)
	}
}
