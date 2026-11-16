// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package main is the root cmd of the provider script.
package main

import (
	"context"
	"crypto/tls"
	_ "embed"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/luthermonson/go-proxmox"
	"github.com/siderolabs/omni/client/pkg/client"
	"github.com/siderolabs/omni/client/pkg/infra"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.yaml.in/yaml/v4"

	"github.com/siderolabs/omni-infra-provider-proxmox/internal/pkg/config"
	"github.com/siderolabs/omni-infra-provider-proxmox/internal/pkg/provider"
	"github.com/siderolabs/omni-infra-provider-proxmox/internal/pkg/provider/meta"
	"github.com/siderolabs/omni-infra-provider-proxmox/internal/version"
)

//go:embed data/schema.json
var schema string

//go:embed data/icon.svg
var icon []byte

// rootCmd represents the base command when called without any subcommands.
var rootCmd = &cobra.Command{
	Use:          "provider",
	Short:        "Proxmox Omni infrastructure provider",
	Long:         `Connects to Omni as an infra provider and manages VMs in Proxmox`,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		loggerConfig := zap.NewProductionConfig()

		logger, err := loggerConfig.Build(
			zap.AddStacktrace(zapcore.ErrorLevel),
		)
		if err != nil {
			return fmt.Errorf("failed to create logger: %w", err)
		}

		if cfg.omniAPIEndpoint == "" {
			return fmt.Errorf("omni-api-endpoint flag is not set")
		}

		if cfg.schemaMode != "basic" && cfg.schemaMode != "advanced" {
			return fmt.Errorf("schema-mode must be basic or advanced")
		}

		if cfg.schemaRefreshInterval < 0 || cfg.machineSetSyncInterval < 0 {
			return fmt.Errorf("refresh intervals cannot be negative; use 0 to disable")
		}

		var proxmoxConfig config.Config

		configFile, err := os.Open(cfg.configFile)
		if err != nil {
			return fmt.Errorf("failed to read Proxmox config file %q: %w", cfg.configFile, err)
		}
		defer configFile.Close() //nolint:errcheck

		decoder := yaml.NewDecoder(configFile)
		decoder.KnownFields(true)

		if err = decoder.Decode(&proxmoxConfig); err != nil {
			return fmt.Errorf("failed to read Proxmox config file %q: %w", cfg.configFile, err)
		}

		if err = proxmoxConfig.Validate(); err != nil {
			return err
		}

		clientOptions := []client.Option{client.WithInsecureSkipTLSVerify(cfg.insecureSkipVerify)}
		if cfg.serviceAccountKey != "" {
			clientOptions = append(clientOptions, client.WithServiceAccount(cfg.serviceAccountKey))
		}

		omniClient, err := client.New(cfg.omniAPIEndpoint, clientOptions...)
		if err != nil {
			return fmt.Errorf("failed to create Omni client: %w", err)
		}
		defer omniClient.Close() //nolint:errcheck

		var opts []proxmox.Option

		switch {
		case proxmoxConfig.Proxmox.Password != "" && proxmoxConfig.Proxmox.Username != "":
			opts = append(opts, proxmox.WithCredentials(&proxmox.Credentials{
				Username: proxmoxConfig.Proxmox.Username,
				Password: proxmoxConfig.Proxmox.Password,
				Realm:    proxmoxConfig.Proxmox.Realm,
			}))
		case proxmoxConfig.Proxmox.TokenID != "" && proxmoxConfig.Proxmox.TokenSecret != "":
			opts = append(opts, proxmox.WithAPIToken(proxmoxConfig.Proxmox.TokenID, proxmoxConfig.Proxmox.TokenSecret))
		}

		transport := &http.Transport{}
		if proxmoxConfig.Proxmox.InsecureSkipVerify {
			transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}

			logger.Info("using insecure connection to Proxmox")
		}

		httpClient := &http.Client{
			Timeout:   time.Second * 30,
			Transport: transport,
		}

		opts = append(opts, proxmox.WithHTTPClient(httpClient))

		proxmoxClient := proxmox.NewClient(
			proxmoxConfig.Proxmox.URL,
			opts...,
		)

		// Discover live cluster resources (nodes, node groups, storages, bridges, SDN vnets, pools, PCI/USB mappings, HA rules, templates)
		// and inject them into the JSON Schema so Omni renders real cluster dropdowns and cascading dependencies.
		baseSchema, err := provider.BuildUISchema(schema, cfg.schemaMode)
		if err != nil {
			return err
		}

		activeSchema := baseSchema
		clusterOpts := provider.DiscoverClusterOptions(cmd.Context(), proxmoxClient, logger, proxmoxConfig.Proxmox.NodeGroups)

		if dynamicSchema, schemaErr := provider.BuildDynamicSchema(baseSchema, clusterOpts); schemaErr != nil {
			logger.Warn("failed to build dynamic schema from Proxmox cluster, falling back to static schema", zap.Error(schemaErr))
		} else {
			activeSchema = dynamicSchema
		}

		provisioner := provider.NewProvisioner(
			proxmoxClient,
			provider.WithMachineState(omniClient.Omni().State()),
			provider.WithDefaultVMIDRange(proxmoxConfig.Proxmox.MinVMID, proxmoxConfig.Proxmox.MaxVMID),
			provider.WithCustomNodeGroups(proxmoxConfig.Proxmox.NodeGroups),
		)

		ip, err := infra.NewProvider(meta.ProviderID, provisioner, infra.ProviderConfig{
			Name:        cfg.providerName,
			Description: cfg.providerDescription,
			Icon:        base64.RawStdEncoding.EncodeToString(icon),
			Schema:      activeSchema,
		})
		if err != nil {
			return fmt.Errorf("failed to create infra provider: %w", err)
		}

		logger.Info("starting infra provider")

		// Periodically refresh the dynamic schema in Omni if the Proxmox cluster topology changes.
		provider.StartSchemaRefresher(cmd.Context(), provider.SchemaRefresherConfig{
			CustomNodeGroups: proxmoxConfig.Proxmox.NodeGroups,
			ProxmoxClient:    proxmoxClient,
			Logger:           logger,
			OmniEndpoint:     cfg.omniAPIEndpoint,
			ProviderID:       meta.ProviderID,
			BaseSchema:       baseSchema,
			LastSchema:       activeSchema,
			ClientOptions:    clientOptions,
			Interval:         cfg.schemaRefreshInterval,
		})

		// Periodically reconcile MachineSet / MachineClass updates onto already-provisioned VMs.
		provider.StartMachineSetUpdater(cmd.Context(), provider.MachineSetUpdaterConfig{
			Provisioner:   provisioner,
			Logger:        logger,
			OmniEndpoint:  cfg.omniAPIEndpoint,
			ProviderID:    meta.ProviderID,
			ClientOptions: clientOptions,
			Interval:      cfg.machineSetSyncInterval,
		})

		healthCheck := func(ctx context.Context) error {
			_, err := proxmoxClient.Version(ctx)

			return err
		}

		return ip.Run(
			cmd.Context(),
			logger,
			infra.WithOmniEndpoint(cfg.omniAPIEndpoint),
			infra.WithClientOptions(clientOptions...),
			infra.WithEncodeRequestIDsIntoTokens(),
			infra.WithVersion(version.Tag),
			infra.WithHealthCheckFunc(healthCheck),
		)
	},
}

var cfg struct {
	omniAPIEndpoint        string
	serviceAccountKey      string
	providerName           string
	providerDescription    string
	configFile             string
	schemaMode             string
	schemaRefreshInterval  time.Duration
	machineSetSyncInterval time.Duration
	insecureSkipVerify     bool
}

func main() {
	if err := app(); err != nil {
		os.Exit(1)
	}
}

func app() error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGHUP, syscall.SIGTERM)
	defer cancel()

	return rootCmd.ExecuteContext(ctx)
}

func init() {
	rootCmd.Flags().StringVar(&cfg.omniAPIEndpoint, "omni-api-endpoint", os.Getenv("OMNI_ENDPOINT"),
		"the endpoint of the Omni API, if not set, defaults to OMNI_ENDPOINT env var.")
	rootCmd.Flags().StringVar(&meta.ProviderID, "id", meta.ProviderID, "the id of the infra provider, it is used to match the resources with the infra provider label.")
	rootCmd.Flags().StringVar(&cfg.serviceAccountKey, "omni-service-account-key", os.Getenv("OMNI_SERVICE_ACCOUNT_KEY"), "Omni infrastructure provider key; defaults to OMNI_SERVICE_ACCOUNT_KEY.")
	rootCmd.Flags().StringVar(&cfg.providerName, "provider-name", "Proxmox", "provider name as it appears in Omni")
	rootCmd.Flags().StringVar(&cfg.providerDescription, "provider-description", "Proxmox infrastructure provider", "Provider description as it appears in Omni")
	rootCmd.Flags().BoolVar(&cfg.insecureSkipVerify, "insecure-skip-verify", false, "ignores untrusted certs on Omni side")
	rootCmd.Flags().DurationVar(&cfg.schemaRefreshInterval, "schema-refresh-interval", 2*time.Minute, "interval for refreshing dynamic Proxmox cluster options in Omni schema (0 to disable)")
	rootCmd.Flags().DurationVar(&cfg.machineSetSyncInterval, "machineset-sync-interval", 0, "opt-in interval for applying MachineRequest changes to existing VMs (0 disables live updates)")

	rootCmd.Flags().StringVar(&cfg.schemaMode, "schema-mode", "basic", "Omni form detail: basic or advanced (existing YAML fields are preserved)")

	// Read everything into this config file
	rootCmd.Flags().StringVar(&cfg.configFile, "config-file", "", "Proxmox provider config")
}
