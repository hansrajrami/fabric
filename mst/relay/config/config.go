// Package config loads the relayer daemon configuration. Everything about
// the MST chain and the Fabric peer is configuration; the relayer's EVM
// private key comes ONLY from the environment (MST_RELAYER_KEY), never from
// the config file, so files can be committed/distributed safely.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/hansrajrami/fabric/mst/relay/capture"
	"github.com/hansrajrami/fabric/mst/relay/evm"
	"github.com/hansrajrami/fabric/mst/relay/sender"
)

// EnvRelayerKey is the environment variable holding the relayer's EVM
// private key (hex).
const EnvRelayerKey = "MST_RELAYER_KEY"

// File is the on-disk JSON shape.
type File struct {
	// OutboxPath is the LevelDB directory for the durable outbox.
	OutboxPath string `json:"outboxPath"`

	Fabric struct {
		Endpoint           string   `json:"endpoint"`
		TLSCACertPath      string   `json:"tlsCACertPath"`
		ServerNameOverride string   `json:"serverNameOverride"`
		MSPID              string   `json:"mspID"`
		CertPath           string   `json:"certPath"`
		KeyPath            string   `json:"keyPath"`
		Channel            string   `json:"channel"`
		DefaultStartBlock  uint64   `json:"defaultStartBlock"`
		ExcludeChaincodes  []string `json:"excludeChaincodes"`
		// AnchorStatusChaincode is the write-back chaincode name. It is
		// force-added to ExcludeChaincodes (echo-loop guard) and used by the
		// write-back client.
		AnchorStatusChaincode string `json:"anchorStatusChaincode"`
	} `json:"fabric"`

	EVM struct {
		RPCURL          string `json:"rpcURL"`
		ContractAddress string `json:"contractAddress"`
		ChainID         uint64 `json:"chainID"`
		GasLimit        uint64 `json:"gasLimit"`
		TipCapGwei      uint64 `json:"tipCapGwei"`
		Confirmations   uint64 `json:"confirmations"`
	} `json:"evm"`

	Sender struct {
		Workers            int    `json:"workers"`
		CadenceMode        string `json:"cadenceMode"` // per-tx | batch | interval
		CadenceN           int    `json:"cadenceN"`
		CadenceIntervalSec int    `json:"cadenceIntervalSeconds"`
		CadenceMaxWaitSec  int    `json:"cadenceMaxWaitSeconds"`
		BackoffMinSec      int    `json:"backoffMinSeconds"`
		BackoffMaxSec      int    `json:"backoffMaxSeconds"`
		ConfirmTimeoutSec  int    `json:"confirmTimeoutSeconds"`
	} `json:"sender"`

	// MetricsAddr serves /metrics (Prometheus text) and /healthz; empty
	// disables the endpoint.
	MetricsAddr string `json:"metricsAddr"`
}

// Load reads and validates the config file.
func Load(path string) (*File, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	var f File
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if f.OutboxPath == "" {
		return nil, fmt.Errorf("config: outboxPath is required")
	}
	if f.EVM.RPCURL == "" || f.EVM.ContractAddress == "" {
		return nil, fmt.Errorf("config: evm.rpcURL and evm.contractAddress are required")
	}
	if f.Fabric.Endpoint == "" || f.Fabric.Channel == "" {
		return nil, fmt.Errorf("config: fabric.endpoint and fabric.channel are required")
	}
	if f.Fabric.AnchorStatusChaincode != "" {
		f.Fabric.ExcludeChaincodes = appendUnique(f.Fabric.ExcludeChaincodes, f.Fabric.AnchorStatusChaincode)
	}
	return &f, nil
}

func appendUnique(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

// GatewayConfig maps to the capture source configuration.
func (f *File) GatewayConfig() capture.GatewayConfig {
	return capture.GatewayConfig{
		Endpoint:           f.Fabric.Endpoint,
		TLSCACertPath:      f.Fabric.TLSCACertPath,
		ServerNameOverride: f.Fabric.ServerNameOverride,
		MSPID:              f.Fabric.MSPID,
		CertPath:           f.Fabric.CertPath,
		KeyPath:            f.Fabric.KeyPath,
		Channel:            f.Fabric.Channel,
	}
}

// CaptureConfig maps to the capture service configuration.
func (f *File) CaptureConfig() capture.Config {
	return capture.Config{
		DefaultStartBlock: f.Fabric.DefaultStartBlock,
		ExcludeChaincodes: f.Fabric.ExcludeChaincodes,
	}
}

// EVMConfig maps to the EVM client configuration; the key is read from the
// environment.
func (f *File) EVMConfig() (evm.Config, error) {
	key := os.Getenv(EnvRelayerKey)
	if key == "" {
		return evm.Config{}, fmt.Errorf("config: %s environment variable is required", EnvRelayerKey)
	}
	return evm.Config{
		RPCURL:          f.EVM.RPCURL,
		ContractAddress: f.EVM.ContractAddress,
		PrivateKeyHex:   key,
		ChainID:         f.EVM.ChainID,
		GasLimit:        f.EVM.GasLimit,
		TipCapGwei:      f.EVM.TipCapGwei,
	}, nil
}

// SenderConfig maps to the sender configuration.
func (f *File) SenderConfig() sender.Config {
	return sender.Config{
		Confirmations: f.EVM.Confirmations,
		Workers:       f.Sender.Workers,
		Cadence: sender.Cadence{
			Mode:     sender.CadenceMode(f.Sender.CadenceMode),
			N:        f.Sender.CadenceN,
			Interval: time.Duration(f.Sender.CadenceIntervalSec) * time.Second,
			MaxWait:  time.Duration(f.Sender.CadenceMaxWaitSec) * time.Second,
		},
		Backoff: sender.Backoff{
			Min: time.Duration(f.Sender.BackoffMinSec) * time.Second,
			Max: time.Duration(f.Sender.BackoffMaxSec) * time.Second,
		},
		ConfirmTimeout: time.Duration(f.Sender.ConfirmTimeoutSec) * time.Second,
	}
}
