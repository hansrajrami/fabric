// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

/// @title  CommitmentCheck
/// @notice Test-only helper: recomputes the MST commitment (spec 6.2) with the
///         EVM's own abi.encode + keccak256, so the cross-language vectors are
///         pinned against what Solidity itself would produce. Never deployed
///         to a real network.
contract CommitmentCheck {
    function compute(
        uint16 schemaVersion,
        bytes32 domainTag,
        bytes32 fabricTxId,
        string calldata channelId,
        string calldata chaincodeId,
        uint64 blockNumber,
        uint64 timestamp,
        bytes32 payloadHash
    ) external pure returns (bytes memory encoded, bytes32 commitment) {
        encoded = abi.encode(
            schemaVersion,
            domainTag,
            fabricTxId,
            channelId,
            chaincodeId,
            blockNumber,
            timestamp,
            payloadHash
        );
        commitment = keccak256(encoded);
    }

    function domainTag() external pure returns (bytes32) {
        return keccak256("MST_FABRIC_TX_ANCHOR_v1");
    }
}
