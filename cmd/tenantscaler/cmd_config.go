package main

import (
	"flag"
	"fmt"
	"os"

	"sigs.k8s.io/yaml"

	"tenantscaler/internal/config"
)

func init() {
	register("config", "validate a config file, or print the default config", runConfig)
}

// loadConfig returns the default config when path is empty.
func loadConfig(path string) (config.Config, error) {
	if path == "" {
		cfg := config.Default()
		return cfg, cfg.Validate()
	}
	return config.Load(path)
}

func runConfig(args []string) error {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	path := fs.String("config", "", "YAML config to validate (empty = built-in default)")
	printCfg := fs.Bool("print", false, "print the effective config as YAML")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := loadConfig(*path)
	if err != nil {
		return err
	}
	if *printCfg {
		out, err := yaml.Marshal(cfg)
		if err != nil {
			return err
		}
		_, err = os.Stdout.Write(out)
		return err
	}
	fmt.Printf("config OK: pool %q capacity %d replicas, %d tenants\n",
		cfg.Pool.Name, cfg.Pool.CapacityReplicas, len(cfg.Tenants))
	return nil
}
