/*
Copyright the MST integration contributors.
SPDX-License-Identifier: Apache-2.0
*/

package node

import (
	"github.com/hyperledger/fabric/core/peer"
	"github.com/hyperledger/fabric/internal/pkg/gateway"
	"github.com/hyperledger/fabric/internal/pkg/mstanchor"
	"github.com/pkg/errors"
	"github.com/spf13/viper"

	"github.com/hansrajrami/fabric/mst/relay/sender"
)

// mstGatewayServer is set by serve() when the embedded gateway is created;
// the MST write-back invokes it in-process (no loopback network hop).
var mstGatewayServer *gateway.Server

// startMSTAnchoring boots the embedded MST anchoring pipeline when
// core.yaml enables it (mst.enabled, default false). Returns nil when
// disabled — the peer then behaves exactly like vanilla Fabric.
func startMSTAnchoring(peerInstance *peer.Peer) (*mstanchor.Service, error) {
	cfg, err := mstanchor.FromViper(viper.GetViper())
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, nil
	}

	var writeback sender.WriteBack
	if cfg.AnchorStatusChaincode != "" {
		if mstGatewayServer == nil {
			return nil, errors.New("mst.anchorStatusChaincode requires the embedded gateway (peer.gateway.enabled) and discovery (peer.discovery.enabled)")
		}
		writeback, err = mstanchor.NewLoopbackWriteBack(
			mstGatewayServer,
			cfg.AnchorStatusChaincode,
			cfg.WriteBack.MSPID,
			cfg.WriteBack.CertPath,
			cfg.WriteBack.KeyPath,
		)
		if err != nil {
			return nil, err
		}
	} else {
		logger.Warning("MST anchoring enabled without mst.anchorStatusChaincode: anchors will not be recorded back on Fabric")
	}

	channels := func() []string {
		infos := peerInstance.GetChannelsInfo()
		ids := make([]string, 0, len(infos))
		for _, info := range infos {
			ids = append(ids, info.GetChannelId())
		}
		return ids
	}

	service := mstanchor.New(cfg, peerInstance, channels, writeback)
	if err := service.Start(); err != nil {
		return nil, err
	}
	return service, nil
}
