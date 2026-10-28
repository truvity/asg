// Command wake-proxy is a small always-on TCP proxy in front of EC2 instances
// that are kept stopped unless somebody uses them. See docs/decisions/0006.
//
//	wake-proxy -config /etc/wake-proxy/config.json
//
// The configuration is a JSON file; see internal/wakeconfig for the fields.
// SIGTERM and SIGINT stop it gracefully.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"

	"github.com/truvity/asg/internal/wakeconfig"
	"github.com/truvity/asg/internal/wakeproxy"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	if err := run(log); err != nil {
		log.Error("wake-proxy failed", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	path := flag.String("config", "", "path of the JSON configuration file")
	flag.Parse()

	if *path == "" {
		return fmt.Errorf("-config is required")
	}

	cfg, err := wakeconfig.Load(*path)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(cfg.Region))
	if err != nil {
		return fmt.Errorf("aws config: %w", err)
	}

	p, err := wakeproxy.New(cfg, wakeproxy.NewEC2(ec2.NewFromConfig(awsCfg)), log, wakeproxy.Options{})
	if err != nil {
		return err
	}

	return p.Run(ctx)
}
