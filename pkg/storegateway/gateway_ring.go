// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/cortexproject/cortex/blob/master/pkg/storegateway/gateway_ring.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Cortex Authors.

package storegateway

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/go-kit/log/level"
	"github.com/grafana/dskit/flagext"
	"github.com/grafana/dskit/kv"

	"github.com/grafana/mimir/pkg/ring"
	util_log "github.com/grafana/mimir/pkg/util/log"
)

const (
	// RingKey is the key under which we store the store gateways ring in the KVStore.
	RingKey = "store-gateway"

	// RingNameForServer is the name of the ring used by the store gateway server.
	RingNameForServer = "store-gateway"

	// RingNameForClient is the name of the ring used by the store gateway client (we need
	// a different name to avoid clashing Prometheus metrics when running in single-binary).
	RingNameForClient = "store-gateway-client"

	// We use a safe default instead of exposing to config option to the user
	// in order to simplify the config.
	RingNumTokens = 512
)

var (
	// BlocksOwnerSync is the operation used to check the authoritative owners of a block
	// (replicas included).
	BlocksOwnerSync = ring.NewOp([]ring.InstanceState{ring.JOINING, ring.ACTIVE, ring.LEAVING}, func(s ring.InstanceState) bool {
		// Extend the replication set only when an instance is LEAVING so that
		// their blocks will be loaded sooner on the next authoritative owner(s).
		return s == ring.LEAVING
	})

	// BlocksOwnerRead is the operation used to check the authoritative owners of a block
	// (replicas included) that are available for queries (a store-gateway is available for
	// queries only when ACTIVE).
	BlocksOwnerRead = ring.NewOp([]ring.InstanceState{ring.ACTIVE}, nil)

	// BlocksRead is the operation run by the querier to query blocks via the store-gateway.
	BlocksRead = ring.NewOp([]ring.InstanceState{ring.ACTIVE}, func(s ring.InstanceState) bool {
		// Blocks can only be queried from ACTIVE instances. However, if the block belongs to
		// a non-active instance, then we should extend the replication set and try to query it
		// from the next ACTIVE instance in the ring (which is expected to have it because a
		// store-gateway keeps their previously owned blocks until new owners are ACTIVE).
		return s != ring.ACTIVE
	})
)

// RingConfig masks the ring lifecycler config which contains
// many options not really required by the store gateways ring. This config
// is used to strip down the config to the minimum, and avoid confusion
// to the user.
type RingConfig struct {
	KVStore              kv.Config     `yaml:"kvstore" doc:"description=The key-value store used to share the hash ring across multiple instances. This option needs be set both on the store-gateway and querier when running in microservices mode."`
	HeartbeatPeriod      time.Duration `yaml:"heartbeat_period"`
	HeartbeatTimeout     time.Duration `yaml:"heartbeat_timeout"`
	ReplicationFactor    int           `yaml:"replication_factor"`
	TokensFilePath       string        `yaml:"tokens_file_path"`
	ZoneAwarenessEnabled bool          `yaml:"zone_awareness_enabled"`

	// Wait ring stability.
	WaitStabilityMinDuration time.Duration `yaml:"wait_stability_min_duration"`
	WaitStabilityMaxDuration time.Duration `yaml:"wait_stability_max_duration"`

	// Instance details
	InstanceID             string   `yaml:"instance_id" doc:"hidden"`
	InstanceInterfaceNames []string `yaml:"instance_interface_names"`
	InstancePort           int      `yaml:"instance_port" doc:"hidden"`
	InstanceAddr           string   `yaml:"instance_addr" doc:"hidden"`
	InstanceZone           string   `yaml:"instance_availability_zone"`

	// Injected internally
	ListenPort      int           `yaml:"-"`
	RingCheckPeriod time.Duration `yaml:"-"`
}

// RegisterFlags adds the flags required to config this to the given FlagSet
func (cfg *RingConfig) RegisterFlags(f *flag.FlagSet) {
	hostname, err := os.Hostname()
	if err != nil {
		level.Error(util_log.Logger).Log("msg", "failed to get hostname", "err", err)
		os.Exit(1)
	}

	ringFlagsPrefix := "store-gateway.sharding-ring."

	// Ring flags
	cfg.KVStore.RegisterFlagsWithPrefix(ringFlagsPrefix, "collectors/", f)
	f.DurationVar(&cfg.HeartbeatPeriod, ringFlagsPrefix+"heartbeat-period", 15*time.Second, "Period at which to heartbeat to the ring. 0 = disabled.")
	f.DurationVar(&cfg.HeartbeatTimeout, ringFlagsPrefix+"heartbeat-timeout", time.Minute, "The heartbeat timeout after which store gateways are considered unhealthy within the ring. 0 = never (timeout disabled)."+sharedOptionWithQuerier)
	f.IntVar(&cfg.ReplicationFactor, ringFlagsPrefix+"replication-factor", 3, "The replication factor to use when sharding blocks."+sharedOptionWithQuerier)
	f.StringVar(&cfg.TokensFilePath, ringFlagsPrefix+"tokens-file-path", "", "File path where tokens are stored. If empty, tokens are not stored at shutdown and restored at startup.")
	f.BoolVar(&cfg.ZoneAwarenessEnabled, ringFlagsPrefix+"zone-awareness-enabled", false, "True to enable zone-awareness and replicate blocks across different availability zones.")

	// Wait stability flags.
	f.DurationVar(&cfg.WaitStabilityMinDuration, ringFlagsPrefix+"wait-stability-min-duration", time.Minute, "Minimum time to wait for ring stability at startup. 0 to disable.")
	f.DurationVar(&cfg.WaitStabilityMaxDuration, ringFlagsPrefix+"wait-stability-max-duration", 5*time.Minute, "Maximum time to wait for ring stability at startup. If the store-gateway ring keeps changing after this period of time, the store-gateway will start anyway.")

	// Instance flags
	cfg.InstanceInterfaceNames = []string{"eth0", "en0"}
	f.Var((*flagext.StringSlice)(&cfg.InstanceInterfaceNames), ringFlagsPrefix+"instance-interface-names", "Name of network interface to read address from.")
	f.StringVar(&cfg.InstanceAddr, ringFlagsPrefix+"instance-addr", "", "IP address to advertise in the ring.")
	f.IntVar(&cfg.InstancePort, ringFlagsPrefix+"instance-port", 0, "Port to advertise in the ring (defaults to server.grpc-listen-port).")
	f.StringVar(&cfg.InstanceID, ringFlagsPrefix+"instance-id", hostname, "Instance ID to register in the ring.")
	f.StringVar(&cfg.InstanceZone, ringFlagsPrefix+"instance-availability-zone", "", "The availability zone where this instance is running. Required if zone-awareness is enabled.")

	// Defaults for internal settings.
	cfg.RingCheckPeriod = 5 * time.Second
}

func (cfg *RingConfig) ToRingConfig() ring.Config {
	rc := ring.Config{}
	flagext.DefaultValues(&rc)

	rc.KVStore = cfg.KVStore
	rc.HeartbeatTimeout = cfg.HeartbeatTimeout
	rc.ReplicationFactor = cfg.ReplicationFactor
	rc.ZoneAwarenessEnabled = cfg.ZoneAwarenessEnabled
	rc.SubringCacheDisabled = true

	return rc
}

func (cfg *RingConfig) ToLifecyclerConfig() (ring.BasicLifecyclerConfig, error) {
	instanceAddr, err := ring.GetInstanceAddr(cfg.InstanceAddr, cfg.InstanceInterfaceNames)
	if err != nil {
		return ring.BasicLifecyclerConfig{}, err
	}

	instancePort := ring.GetInstancePort(cfg.InstancePort, cfg.ListenPort)

	return ring.BasicLifecyclerConfig{
		ID:                  cfg.InstanceID,
		Addr:                fmt.Sprintf("%s:%d", instanceAddr, instancePort),
		Zone:                cfg.InstanceZone,
		HeartbeatPeriod:     cfg.HeartbeatPeriod,
		TokensObservePeriod: 0,
		NumTokens:           RingNumTokens,
	}, nil
}
