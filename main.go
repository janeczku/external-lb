package main

import (
	"flag"
	"github.com/Sirupsen/logrus"
	"github.com/rancher/external-lb/metadata"
	"github.com/rancher/external-lb/model"
	"github.com/rancher/external-lb/providers"
	_ "github.com/rancher/external-lb/providers/elbv2"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"
)

const (
	defaultPollInterval = 1000
	forceUpdateInterval = 1
)

var (
	providerName = flag.String("provider", "elbv2", "External LB provider name")
	debug        = flag.Bool("debug", false, "Debug")
	logFile      = flag.String("log", "", "Log file")

	pollInterval int
	provider     providers.Provider
	m            *metadata.MetadataClient
	c            *CattleClient

	metadataLBConfigsCached = make(map[string]model.LBConfig)
)

func setEnv() {
	flag.Parse()
	if *debug {
		logrus.SetLevel(logrus.DebugLevel)
	}

	if *logFile != "" {
		if output, err := os.OpenFile(*logFile, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666); err != nil {
			logrus.Fatalf("Failed to log to file %s: %v", *logFile, err)
		} else {
			logrus.SetOutput(output)
			formatter := &logrus.TextFormatter{
				FullTimestamp: true,
			}
			logrus.SetFormatter(formatter)
		}
	}

	var err error
	if env := os.Getenv("LB_POLL_INTERVAL"); len(env) > 0 {
		pollInterval, err = strconv.Atoi(env)
		if err != nil {
			logrus.Fatalf("Failed to parse LB_POLL_INTERVAL to integer: %v", err)
		}
	} else {
		logrus.Infof("Environment variable 'LB_POLL_INTERVAL' not set. "+
			"Using default interval %d", defaultPollInterval)
		pollInterval = defaultPollInterval
	}

	// initialize metadata client
	m, err = metadata.NewMetadataClient()
	if err != nil {
		logrus.Fatalf("Failed to initialize Rancher metadata client: %v", err)
	}

	// initialize cattle client
	c, err = NewCattleClientFromEnvironment()
	if err != nil {
		logrus.Fatalf("Failed to initialize Rancher API client: %v", err)
	}

	// initialize provider
	provider, err = providers.GetProvider(*providerName)
	if err != nil {
		logrus.Fatalf("Failed to initialize provider '%s': %v", *providerName, err)
	}
}

func main() {
	logrus.Infof("Starting Rancher External LoadBalancer service")
	setEnv()

	go startHealthcheck()

	version := "init"
	lastUpdated := time.Now()

	ticker := time.NewTicker(time.Duration(pollInterval) * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		update, updateForced := false, false
		newVersion, err := m.GetVersion()
		if err != nil {
			logrus.Errorf("Failed to get metadata version: %v", err)
		} else if version != newVersion {
			logrus.Debugf("Metadata version changed. Old: %s New: %s.", version, newVersion)
			version = newVersion
			update = true
		} else {
			if time.Since(lastUpdated).Minutes() >= forceUpdateInterval {
				logrus.Debugf("Executing force update as metadata version hasn't changed in: %d minutes",
					forceUpdateInterval)
				updateForced = true
			}
		}

		if update || updateForced {
			// get records from metadata
			metadataLBConfigs, err := m.GetMetadataLBConfigs()
			if err != nil {
				logrus.Errorf("Failed to get LB configs from metadata: %v", err)
				continue
			}

			logrus.Debugf("LB configs from metadata: %v", metadataLBConfigs)

			// A flapping service might cause the metadata version to change
			// in short intervals. Caching the previous LB Configs allows
			// us to check if the actual LB Configs have changed, so we
			// don't end up flooding the provider with unnecessary requests.
			if !reflect.DeepEqual(metadataLBConfigs, metadataLBConfigsCached) || updateForced {
				// update the provider
				updatedFqdn, err := UpdateProviderLBConfigs(metadataLBConfigs)
				if err != nil {
					logrus.Errorf("Failed to update provider: %v", err)
				}

				// update the service FQDN in Cattle
				for fqdn, config := range updatedFqdn {
					for _, fe := range config.Frontends {
						for _, tp := range fe.TargetPools {
							// service_stack_environment
							parts := strings.Split(tp.Name, "_")
							err := c.UpdateServiceFqdn(parts[0], parts[1], fqdn)
							if err != nil {
								logrus.Errorf("Failed to update service FQDN: %v", err)
							}
						}
					}
				}

				metadataLBConfigsCached = metadataLBConfigs
				lastUpdated = time.Now()
			} else {
				logrus.Debugf("LB configs from metadata did not change")
			}
		}
	}
}
