/*
Copyright the MST integration contributors.
SPDX-License-Identifier: Apache-2.0
*/

// Package mstanchor embeds the MST proof-anchoring pipeline into the peer
// binary, behind the core.yaml `mst.enabled` flag (default off). It reuses
// the exact same proto-free capture core, durable outbox, and sender that
// the standalone mst-relayd daemon uses; only the block source (the peer's
// own ledger instead of the Gateway event API) and the configuration
// surface (core.yaml instead of a JSON file) differ.
//
// The commit path remains untouched even in embedded mode: blocks are read
// through the ledger's blocks iterator strictly AFTER commit, and every
// MST-facing operation is asynchronous behind the crash-safe outbox.
package mstanchor

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/viper"

	"github.com/hansrajrami/fabric/mst/relay/capture"
	"github.com/hansrajrami/fabric/mst/relay/evm"
	"github.com/hansrajrami/fabric/mst/relay/sender"
)

// EnvRelayerKey is the environment variable holding the relayer's EVM
// private key (hex). It is intentionally NOT a core.yaml value.
const EnvRelayerKey = "MST_RELAYER_KEY"

// Config is the embedded relayer configuration, read from the peer's
// core.yaml `mst:` section.
type Config struct {
	Enabled bool
	// OutboxPath is the root directory for the durable outboxes (one
	// subdirectory per channel).
	OutboxPath string
	// Channels restricts anchoring to these channels; empty means every
	// channel the peer has joined.
	Channels []string
	// DefaultStartBlock applies when a channel has no checkpoint yet.
	DefaultStartBlock uint64
	// CaptureMode: "opt-in" (default) anchors only MSTProofRequest
	// emitters; "all" anchors every valid transaction (non-opted txs get
	// the empty payload hash — an existence proof). Batch cadence is
	// strongly advisable with "all".
	CaptureMode string
	// IncludeChaincodes restricts CaptureMode "all" to these chaincodes
	// (empty = every chaincode). Ignored in opt-in mode.
	IncludeChaincodes []string
	// ExcludeChaincodes are never captured; AnchorStatusChaincode is always
	// added (echo-loop guard).
	ExcludeChaincodes     []string
	AnchorStatusChaincode string

	EVM struct {
		RPCURL          string
		ContractAddress string
		ChainID         uint64
		GasLimit        uint64
		TipCapGwei      uint64
		Confirmations   uint64
		// MinBalanceGwei: log loudly when the relayer's gas balance drops
		// below this (0 disables the watcher; the metrics gauge is always
		// exposed regardless).
		MinBalanceGwei uint64
	}

	Sender struct {
		Workers int
		// BatchStrategy: "individual" (default) or "merkle" (one root per
		// flush; verifiers need inclusion proofs from mst-proof).
		BatchStrategy   string
		CadenceMode     string // per-tx | batch | interval | cron
		CadenceN        int
		CadenceInterval time.Duration
		CadenceMaxWait  time.Duration
		CadenceCron     string
		BackoffMin      time.Duration
		BackoffMax      time.Duration
		ConfirmTimeout  time.Duration
	}

	// Outbox backend AUTO-FOLLOWS the peer's ledger.state.stateDatabase:
	// goleveldb -> embedded LevelDB under OutboxPath; CouchDB -> dedicated
	// per-channel databases on the peer's own CouchDB server (never the
	// peer's state databases themselves). Connection settings come from
	// ledger.state.couchDBConfig, overridable via mst.outbox.couchDB.*.
	Outbox struct {
		Backend string // "leveldb" | "couchdb"
		CouchDB struct {
			Address  string
			Username string
			Password string
		}
	}

	// WriteBack is the relayer's Fabric identity used to submit RecordAnchor
	// through the peer's embedded gateway. Required when
	// AnchorStatusChaincode is set.
	WriteBack struct {
		MSPID    string
		CertPath string
		KeyPath  string
	}

	// MetricsAddr serves /metrics and /healthz for the embedded relayer;
	// empty disables it (the peer's operations endpoint is separate).
	MetricsAddr string
}

// FromViper reads the mst.* section from the peer's configuration. With
// mst.enabled absent or false, an all-zero disabled config is returned and
// the peer behaves exactly like vanilla Fabric.
func FromViper(v *viper.Viper) (*Config, error) {
	c := &Config{}
	c.Enabled = v.GetBool("mst.enabled")
	if !c.Enabled {
		return c, nil
	}

	c.OutboxPath = v.GetString("mst.outboxPath")
	c.Channels = v.GetStringSlice("mst.channels")
	c.DefaultStartBlock = uint64(v.GetInt64("mst.defaultStartBlock"))
	c.CaptureMode = v.GetString("mst.captureMode")
	mode := capture.Mode(c.CaptureMode)
	if err := mode.Validate(); err != nil {
		return nil, fmt.Errorf("mstanchor: %w", err)
	}
	c.CaptureMode = string(mode)
	c.IncludeChaincodes = v.GetStringSlice("mst.includeChaincodes")
	c.ExcludeChaincodes = v.GetStringSlice("mst.excludeChaincodes")
	c.AnchorStatusChaincode = v.GetString("mst.anchorStatusChaincode")

	c.EVM.RPCURL = v.GetString("mst.evm.rpcURL")
	c.EVM.ContractAddress = v.GetString("mst.evm.contractAddress")
	c.EVM.ChainID = uint64(v.GetInt64("mst.evm.chainID"))
	c.EVM.GasLimit = uint64(v.GetInt64("mst.evm.gasLimit"))
	c.EVM.TipCapGwei = uint64(v.GetInt64("mst.evm.tipCapGwei"))
	c.EVM.Confirmations = uint64(v.GetInt64("mst.evm.confirmations"))
	c.EVM.MinBalanceGwei = uint64(v.GetInt64("mst.evm.minBalanceGwei"))

	c.Sender.Workers = v.GetInt("mst.sender.workers")
	c.Sender.BatchStrategy = v.GetString("mst.sender.batchStrategy")
	c.Sender.CadenceMode = v.GetString("mst.sender.cadenceMode")
	c.Sender.CadenceCron = v.GetString("mst.sender.cadenceCron")
	c.Sender.CadenceN = v.GetInt("mst.sender.cadenceN")
	c.Sender.CadenceInterval = v.GetDuration("mst.sender.cadenceInterval")
	c.Sender.CadenceMaxWait = v.GetDuration("mst.sender.cadenceMaxWait")
	c.Sender.BackoffMin = v.GetDuration("mst.sender.backoffMin")
	c.Sender.BackoffMax = v.GetDuration("mst.sender.backoffMax")
	c.Sender.ConfirmTimeout = v.GetDuration("mst.sender.confirmTimeout")

	// Auto-follow the peer's state database choice.
	c.Outbox.Backend = "leveldb"
	if strings.EqualFold(v.GetString("ledger.state.stateDatabase"), "CouchDB") {
		c.Outbox.Backend = "couchdb"
		c.Outbox.CouchDB.Address = firstNonEmpty(
			v.GetString("mst.outbox.couchDB.address"),
			v.GetString("ledger.state.couchDBConfig.couchDBAddress"),
		)
		c.Outbox.CouchDB.Username = firstNonEmpty(
			v.GetString("mst.outbox.couchDB.username"),
			v.GetString("ledger.state.couchDBConfig.username"),
		)
		c.Outbox.CouchDB.Password = firstNonEmpty(
			v.GetString("mst.outbox.couchDB.password"),
			v.GetString("ledger.state.couchDBConfig.password"),
		)
		if c.Outbox.CouchDB.Address == "" {
			return nil, fmt.Errorf("mstanchor: state database is CouchDB but no couchDB address is configured")
		}
	}

	c.WriteBack.MSPID = v.GetString("mst.writeback.mspID")
	c.WriteBack.CertPath = v.GetString("mst.writeback.certPath")
	c.WriteBack.KeyPath = v.GetString("mst.writeback.keyPath")

	c.MetricsAddr = v.GetString("mst.metricsAddr")

	if c.OutboxPath == "" {
		return nil, fmt.Errorf("mstanchor: mst.outboxPath is required when mst.enabled is true")
	}
	if c.EVM.RPCURL == "" || c.EVM.ContractAddress == "" {
		return nil, fmt.Errorf("mstanchor: mst.evm.rpcURL and mst.evm.contractAddress are required when mst.enabled is true")
	}
	if c.AnchorStatusChaincode != "" {
		c.ExcludeChaincodes = appendUnique(c.ExcludeChaincodes, c.AnchorStatusChaincode)
	}
	return c, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// couchDatabaseName derives the per-channel outbox database name. Channel
// ids match [a-z][a-z0-9.-]* and CouchDB forbids '.', so dots map to '_'
// (which cannot occur in channel ids — no collisions).
func couchDatabaseName(channelID string) string {
	return "mst_outbox_" + strings.ReplaceAll(channelID, ".", "_")
}

func appendUnique(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

// EVMConfig maps to the shared EVM client config; the key comes strictly
// from the environment.
func (c *Config) EVMConfig() (evm.Config, error) {
	key := os.Getenv(EnvRelayerKey)
	if key == "" {
		return evm.Config{}, fmt.Errorf("mstanchor: %s environment variable is required when mst.enabled is true", EnvRelayerKey)
	}
	return evm.Config{
		RPCURL:          c.EVM.RPCURL,
		ContractAddress: c.EVM.ContractAddress,
		PrivateKeyHex:   key,
		ChainID:         c.EVM.ChainID,
		GasLimit:        c.EVM.GasLimit,
		TipCapGwei:      c.EVM.TipCapGwei,
	}, nil
}

// CaptureConfig maps to the shared capture config.
func (c *Config) CaptureConfig() capture.Config {
	return capture.Config{
		Mode:              capture.Mode(c.CaptureMode),
		DefaultStartBlock: c.DefaultStartBlock,
		ExcludeChaincodes: c.ExcludeChaincodes,
		IncludeChaincodes: c.IncludeChaincodes,
	}
}

// SenderConfig maps to the shared sender config.
func (c *Config) SenderConfig() sender.Config {
	return sender.Config{
		Confirmations: c.EVM.Confirmations,
		Workers:       c.Sender.Workers,
		Strategy:      sender.BatchStrategy(c.Sender.BatchStrategy),
		Cadence: sender.Cadence{
			Mode:     sender.CadenceMode(c.Sender.CadenceMode),
			N:        c.Sender.CadenceN,
			Interval: c.Sender.CadenceInterval,
			MaxWait:  c.Sender.CadenceMaxWait,
			Cron:     c.Sender.CadenceCron,
		},
		Backoff: sender.Backoff{
			Min: c.Sender.BackoffMin,
			Max: c.Sender.BackoffMax,
		},
		ConfirmTimeout: c.Sender.ConfirmTimeout,
	}
}

// channelAllowed reports whether the channel participates in anchoring.
func (c *Config) channelAllowed(channelID string) bool {
	if len(c.Channels) == 0 {
		return true
	}
	for _, ch := range c.Channels {
		if ch == channelID {
			return true
		}
	}
	return false
}
