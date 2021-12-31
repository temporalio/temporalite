// Unless explicitly stated otherwise all files in this repository are licensed under the MIT License.
//
// This product includes software developed at Datadog (https://www.datadoghq.com/). Copyright 2021 Datadog, Inc.

package main

import (
	"fmt"
	goLog "log"
	"net"
	"os"
	"strings"

	uiserver "github.com/temporalio/ui-server/server"
	uiconfig "github.com/temporalio/ui-server/server/config"
	uiserveroptions "github.com/temporalio/ui-server/server/server_options"
	"github.com/urfave/cli/v2"
	"go.temporal.io/server/common/headers"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/temporal"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	// Load sqlite storage driver
	_ "go.temporal.io/server/common/persistence/sql/sqlplugin/sqlite"

	"github.com/DataDog/temporalite"
	"github.com/DataDog/temporalite/internal/liteconfig"
)

var (
	defaultCfg *liteconfig.Config
)

const (
	ephemeralFlag = "ephemeral"
	dbPathFlag    = "filename"
	portFlag      = "port"
	uiPortFlag    = "ui-port"
	ipFlag        = "ip"
	logFormatFlag = "log-format"
	namespaceFlag = "namespace"
	pragmaFlag    = "sqlite-pragma"
)

func init() {
	defaultCfg, _ = liteconfig.NewDefaultConfig()
}

func main() {
	if err := buildCLI().Run(os.Args); err != nil {
		goLog.Fatal(err)
	}
}

func buildCLI() *cli.App {
	app := cli.NewApp()
	app.Name = "temporal"
	app.Usage = "Temporal server"
	app.Version = headers.ServerVersion
	app.Commands = []*cli.Command{
		{
			Name:      "start",
			Usage:     "Start Temporal server",
			ArgsUsage: " ",
			Flags: []cli.Flag{
				&cli.BoolFlag{
					Name:  ephemeralFlag,
					Value: defaultCfg.Ephemeral,
					Usage: "enable the in-memory storage driver **data will be lost on restart**",
				},
				&cli.StringFlag{
					Name:    dbPathFlag,
					Aliases: []string{"f"},
					Value:   defaultCfg.DatabaseFilePath,
					Usage:   "file in which to persist Temporal state",
				},
				&cli.StringSliceFlag{
					Name:    namespaceFlag,
					Aliases: []string{"n"},
					Usage:   `specify namespaces that should be pre-created`,
					EnvVars: nil,
					Value:   nil,
				},
				&cli.IntFlag{
					Name:    portFlag,
					Aliases: []string{"p"},
					Usage:   "port for the temporal-frontend GRPC service",
					Value:   liteconfig.DefaultFrontendPort,
				},
				&cli.IntFlag{
					Name:        uiPortFlag,
					Usage:       "port for the temporal web UI",
					DefaultText: fmt.Sprintf("--port + 1000, eg. %d", liteconfig.DefaultFrontendPort+1000),
				},
				&cli.StringFlag{
					Name:    ipFlag,
					Usage:   `IPv4 address to bind the frontend service to instead of localhost`,
					EnvVars: nil,
					Value:   "127.0.0.1",
				},
				&cli.StringFlag{
					Name:    logFormatFlag,
					Usage:   `customize the log formatting (allowed: ["json" "pretty"])`,
					EnvVars: nil,
					Value:   "json",
				},
				&cli.StringSliceFlag{
					Name:    pragmaFlag,
					Aliases: []string{"sp"},
					Usage:   fmt.Sprintf("specify sqlite pragma statements in pragma=value format (allowed: %q)", liteconfig.GetAllowedPragmas()),
					EnvVars: nil,
					Value:   nil,
				},
			},
			Before: func(c *cli.Context) error {
				if c.Args().Len() > 0 {
					return cli.Exit("ERROR: start command doesn't support arguments.", 1)
				}
				if c.IsSet(ephemeralFlag) && c.IsSet(dbPathFlag) {
					return cli.Exit(fmt.Sprintf("ERROR: only one of %q or %q flags may be passed at a time", ephemeralFlag, dbPathFlag), 1)
				}

				switch c.String(logFormatFlag) {
				case "json", "pretty":
				default:
					return cli.Exit(fmt.Sprintf("bad value %q passed for flag %q", c.String(logFormatFlag), logFormatFlag), 1)
				}

				// Check that ip address is valid
				if c.IsSet(ipFlag) && net.ParseIP(c.String(ipFlag)) == nil {
					return cli.Exit(fmt.Sprintf("bad value %q passed for flag %q", c.String(ipFlag), ipFlag), 1)
				}

				return nil
			},
			Action: func(c *cli.Context) error {
				var (
					ip         = c.String(ipFlag)
					serverPort = c.Int(portFlag)
					uiPort     = serverPort + 1000
				)

				if c.IsSet(uiPortFlag) {
					uiPort = c.Int(uiPortFlag)
				}
				uiOpts := uiconfig.Config{
					TemporalGRPCAddress: fmt.Sprintf(":%d", c.Int(portFlag)),
					Host:                ip,
					Port:                uiPort,
					EnableUI:            true,
				}

				pragmas, err := getPragmaMap(c.StringSlice(pragmaFlag))
				if err != nil {
					return err
				}

				opts := []temporalite.ServerOption{
					temporalite.WithFrontendPort(serverPort),
					temporalite.WithFrontendIP(ip),
					temporalite.WithDatabaseFilePath(c.String(dbPathFlag)),
					temporalite.WithNamespaces(c.StringSlice(namespaceFlag)...),
					temporalite.WithSQLitePragmas(pragmas),
					temporalite.WithUpstreamOptions(
						temporal.InterruptOn(temporal.InterruptCh()),
					),
					temporalite.WithUI(uiserver.NewServer(uiserveroptions.WithConfig(&uiOpts))),
				}
				if c.Bool(ephemeralFlag) {
					opts = append(opts, temporalite.WithPersistenceDisabled())
				}
				if c.String(logFormatFlag) == "pretty" {
					lcfg := zap.NewDevelopmentConfig()
					lcfg.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
					l, err := lcfg.Build(
						zap.WithCaller(false),
						zap.AddStacktrace(zapcore.ErrorLevel),
					)
					if err != nil {
						return err
					}
					logger := log.NewZapLogger(l)
					opts = append(opts, temporalite.WithLogger(logger))
				}

				s, err := temporalite.NewServer(opts...)
				if err != nil {
					return err
				}

				if err := s.Start(); err != nil {
					return cli.Exit(fmt.Sprintf("Unable to start server. Error: %v", err), 1)
				}
				return cli.Exit("All services are stopped.", 0)
			},
		},
	}

	return app
}

func getPragmaMap(input []string) (map[string]string, error) {
	result := make(map[string]string)
	for _, pragma := range input {
		vals := strings.Split(pragma, "=")
		if len(vals) != 2 {
			return nil, fmt.Errorf("ERROR: pragma statements must be in KEY=VALUE format, got %q", pragma)
		}
		result[vals[0]] = vals[1]
	}
	return result, nil
}
