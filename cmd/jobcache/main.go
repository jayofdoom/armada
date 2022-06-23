package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
	"golang.org/x/sync/errgroup"

	"github.com/G-Research/armada/internal/common"
	"github.com/G-Research/armada/internal/jobcache"
	"github.com/G-Research/armada/internal/jobcache/configuration"
)

const CustomConfigLocation string = "config"

func init() {
	pflag.StringSlice(CustomConfigLocation, []string{}, "Fully qualified path to application configuration file (for multiple config files repeat this arg or separate paths with commas)")
	pflag.Parse()
}

func main() {
	common.ConfigureLogging()
	common.BindCommandlineArguments()
	g, ctx := errgroup.WithContext(context.Background())

	var config configuration.JobCacheConfiguration
	userSpecifiedConfigs := viper.GetStringSlice(CustomConfigLocation)
	common.LoadConfig(&config, "./config/jobcache", userSpecifiedConfigs)

	shutdown, wg := jobcache.StartUp(&config)

	// Cancel the errgroup context on SIGINT and SIGTERM,
	// which shuts everything down gracefully.
	stopSignal := make(chan os.Signal, 1)
	signal.Notify(stopSignal, syscall.SIGINT, syscall.SIGTERM)
	g.Go(func() error {
		select {
		case <-ctx.Done():
			return nil
		case sig := <-stopSignal:
			wg.Done()
			shutdown()
			return fmt.Errorf("received signal %v", sig)
		}
	})

	wg.Wait()

}
