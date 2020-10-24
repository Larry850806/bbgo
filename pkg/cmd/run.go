package cmd

import (
	"bytes"
	"context"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"text/template"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	flag "github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/c9s/bbgo/pkg/bbgo"
	"github.com/c9s/bbgo/pkg/cmd/cmdutil"
	"github.com/c9s/bbgo/pkg/config"
	"github.com/c9s/bbgo/pkg/notifier/slacknotifier"
	"github.com/c9s/bbgo/pkg/slack/slacklog"

	// import built-in strategies
	_ "github.com/c9s/bbgo/pkg/strategy/buyandhold"
)

var errSlackTokenUndefined = errors.New("slack token is not defined.")

func init() {
	RunCmd.Flags().Bool("no-compile", false, "do not compile wrapper binary")
	RunCmd.Flags().String("config", "config/bbgo.yaml", "strategy config file")
	RunCmd.Flags().String("since", "", "pnl since time")
	RootCmd.AddCommand(RunCmd)
}

var runTemplate = template.Must(template.New("main").Parse(`package main
// DO NOT MODIFY THIS FILE. THIS FILE IS GENERATED FOR IMPORTING STRATEGIES
import (
	"github.com/c9s/bbgo/pkg/cmd"

{{- range .Imports }}
	_ "{{ . }}"
{{- end }}
)

func main() {
	cmd.Execute()
}

`))

func compileRunFile(filepath string, config *config.Config) error {
	var buf = bytes.NewBuffer(nil)
	if err := runTemplate.Execute(buf, config); err != nil {
		return err
	}

	return ioutil.WriteFile(filepath, buf.Bytes(), 0644)
}

func runConfig(ctx context.Context, config *config.Config) error {
	slackToken := viper.GetString("slack-token")
	if len(slackToken) == 0 {
		return errSlackTokenUndefined
	}

	logrus.AddHook(slacklog.NewLogHook(slackToken, viper.GetString("slack-error-channel")))

	var notifier = slacknotifier.New(slackToken, viper.GetString("slack-channel"))

	db, err := cmdutil.ConnectMySQL()
	if err != nil {
		return err
	}

	environ := bbgo.NewDefaultEnvironment(db)
	environ.ReportTrade(notifier)

	trader := bbgo.NewTrader(environ)

	for _, entry := range config.ExchangeStrategies {
		for _, mount := range entry.Mounts {
			logrus.Infof("attaching strategy %T on %s...", entry.Strategy, mount)
			trader.AttachStrategyOn(mount, entry.Strategy)
		}
	}

	for _, strategy := range config.CrossExchangeStrategies {
		logrus.Infof("attaching strategy %T", strategy)
		trader.AttachCrossExchangeStrategy(strategy)
	}

	for _, report := range config.PnLReporters {
		if len(report.AverageCostBySymbols) > 0 {
			trader.ReportPnL(notifier).
				AverageCostBySymbols(report.AverageCostBySymbols...).
				Of(report.Of...).
				When(report.When...)
		} else {
			return errors.Errorf("unsupported PnL reporter: %+v", report)
		}
	}

	return trader.Run(ctx)
}

var RunCmd = &cobra.Command{
	Use:   "run",
	Short: "run strategies from config file",

	// SilenceUsage is an option to silence usage when an error occurs.
	SilenceUsage: true,

	RunE: func(cmd *cobra.Command, args []string) error {
		configFile, err := cmd.Flags().GetString("config")
		if err != nil {
			return err
		}

		if len(configFile) == 0 {
			return errors.New("--config option is required")
		}

		noCompile, err := cmd.Flags().GetBool("no-compile")
		if err != nil {
			return err
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		userConfig, err := config.Load(configFile)
		if err != nil {
			return err
		}

		if noCompile {
			if err := runConfig(ctx, userConfig); err != nil {
				return err
			}
			cmdutil.WaitForSignal(ctx, syscall.SIGINT, syscall.SIGTERM)
			return nil
		} else {
			buildDir := filepath.Join("build", "bbgow")
			if _, err := os.Stat(buildDir); os.IsNotExist(err) {
				if err := os.MkdirAll(buildDir, 0777); err != nil {
					return errors.Wrapf(err, "can not create build directory: %s", buildDir)
				}
			}

			mainFile := filepath.Join(buildDir, "main.go")
			if err := compileRunFile(mainFile, userConfig); err != nil {
				return errors.Wrap(err, "compile error")
			}

			// TODO: use "\" for Windows
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}

			buildTarget := filepath.Join(cwd, buildDir)
			logrus.Infof("building binary from %s...", buildTarget)

			buildCmd := exec.CommandContext(ctx, "go", "build", "-tags", "wrapper", "-o", "bbgow", buildTarget)
			buildCmd.Stdout = os.Stdout
			buildCmd.Stderr = os.Stderr
			if err := buildCmd.Run(); err != nil {
				return err
			}

			var flagsArgs = []string{"run", "--no-compile"}
			cmd.Flags().Visit(func(flag *flag.Flag) {
				flagsArgs = append(flagsArgs, flag.Name, flag.Value.String())
			})
			flagsArgs = append(flagsArgs, args...)

			executePath := filepath.Join(cwd, "bbgow")
			runCmd := exec.CommandContext(ctx, executePath, flagsArgs...)
			runCmd.Stdout = os.Stdout
			runCmd.Stderr = os.Stderr
			if err := runCmd.Run(); err != nil {
				return err
			}

		}

		return nil
	},
}
