// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

/// @title  IMSTAnchor — public, permanent record of Fabric tx commitments.
interface IMSTAnchor {
    event Anchored(
        bytes32 indexed fabricTxId,
        bytes32 commitment,
        uint64 blockNumber,
        uint64 evmTimestamp
    );

    event RootAnchored(bytes32 indexed root, uint64 leafCount, uint64 evmTimestamp);

    /// @notice Record a commitment for a Fabric tx. Idempotent: a second call for an
    ///         already-anchored fabricTxId is a quiet no-op (no revert, no overwrite).
    function anchor(bytes32 fabricTxId, bytes32 commitment, uint64 blockNumber) external;

    /// @notice Record a Merkle batch root aggregating many commitments (the
    ///         proof-of-combination). Same idempotency: re-anchoring an
    ///         existing root is a quiet no-op.
    function anchorRoot(bytes32 root, uint64 leafCount) external;

    function getAnchor(bytes32 fabricTxId)
        external
        view
        returns (bytes32 commitment, uint64 blockNumber, uint64 evmTimestamp, bool exists);

    function getRoot(bytes32 root)
        external
        view
        returns (uint64 leafCount, uint64 evmTimestamp, bool exists);
}

/// @title  MSTAnchor
/// @notice Maps `fabricTxId -> commitment`, immutable per key, emitting `Anchored` on
///         first write. Duplicate submissions are quiet no-ops, which makes at-least-once
///         delivery from the relayer exactly-once in effect. The contract stamps
///         `evmTimestamp` from `block.timestamp` itself; the Fabric commit time is
///         already inside the commitment, so no caller-supplied time is trusted.
///
///         Writes MAY be restricted to known relayer addresses (mild integrity guard,
///         fixed at deploy time via `allowlistEnabled`). This is not the security model:
///         a commitment is meaningless to forge without the real Fabric data.
contract MSTAnchor is IMSTAnchor {
    struct Anchor {
        bytes32 commitment; // slot n
        uint64 blockNumber; // slot n+1 ─┐
        uint64 evmTimestamp; //          ├─ packed into one slot
        bool exists; //                  ─┘
    }

    error NotOwner();
    error NotRelayer();
    error ZeroAddress();

    event RelayerSet(address indexed relayer, bool allowed);
    event OwnershipTransferred(address indexed previousOwner, address indexed newOwner);

    struct RootRecord {
        uint64 leafCount; //  ─┐
        uint64 evmTimestamp; //├─ packed into one slot
        bool exists; //        ─┘
    }

    address public owner;
    bool public immutable allowlistEnabled;
    mapping(address => bool) public isRelayer;
    mapping(bytes32 => Anchor) private _anchors;
    mapping(bytes32 => RootRecord) private _roots;

    modifier onlyOwner() {
        if (msg.sender != owner) revert NotOwner();
        _;
    }

    constructor(bool allowlistEnabled_, address[] memory initialRelayers) {
        owner = msg.sender;
        allowlistEnabled = allowlistEnabled_;
        for (uint256 i = 0; i < initialRelayers.length; ++i) {
            address relayer = initialRelayers[i];
            if (relayer == address(0)) revert ZeroAddress();
            isRelayer[relayer] = true;
            emit RelayerSet(relayer, true);
        }
        emit OwnershipTransferred(address(0), msg.sender);
    }

    /// @inheritdoc IMSTAnchor
    function anchor(bytes32 fabricTxId, bytes32 commitment, uint64 blockNumber) external {
        if (allowlistEnabled && !isRelayer[msg.sender]) revert NotRelayer();
        _anchor(fabricTxId, commitment, blockNumber);
    }

    /// @notice Anchor several commitments in one transaction (gas-efficient for the
    ///         every-N-transactions cadence). Per-key idempotency is preserved: already
    ///         anchored ids are skipped quietly, the rest are recorded.
    function anchorBatch(
        bytes32[] calldata fabricTxIds,
        bytes32[] calldata commitments,
        uint64[] calldata blockNumbers
    ) external {
        if (allowlistEnabled && !isRelayer[msg.sender]) revert NotRelayer();
        uint256 n = fabricTxIds.length;
        require(commitments.length == n && blockNumbers.length == n, "length mismatch");
        for (uint256 i = 0; i < n; ++i) {
            _anchor(fabricTxIds[i], commitments[i], blockNumbers[i]);
        }
    }

    function _anchor(bytes32 fabricTxId, bytes32 commitment, uint64 blockNumber) internal {
        Anchor storage a = _anchors[fabricTxId];
        if (a.exists) return; // immutable per key: quiet no-op, no overwrite, no event
        uint64 nowTs = uint64(block.timestamp);
        a.commitment = commitment;
        a.blockNumber = blockNumber;
        a.evmTimestamp = nowTs;
        a.exists = true;
        emit Anchored(fabricTxId, commitment, blockNumber, nowTs);
    }

    /// @inheritdoc IMSTAnchor
    function anchorRoot(bytes32 root, uint64 leafCount) external {
        if (allowlistEnabled && !isRelayer[msg.sender]) revert NotRelayer();
        RootRecord storage r = _roots[root];
        if (r.exists) return; // immutable per root: quiet no-op
        uint64 nowTs = uint64(block.timestamp);
        r.leafCount = leafCount;
        r.evmTimestamp = nowTs;
        r.exists = true;
        emit RootAnchored(root, leafCount, nowTs);
    }

    /// @inheritdoc IMSTAnchor
    function getRoot(bytes32 root)
        external
        view
        returns (uint64 leafCount, uint64 evmTimestamp, bool exists)
    {
        RootRecord storage r = _roots[root];
        return (r.leafCount, r.evmTimestamp, r.exists);
    }

    /// @inheritdoc IMSTAnchor
    function getAnchor(bytes32 fabricTxId)
        external
        view
        returns (bytes32 commitment, uint64 blockNumber, uint64 evmTimestamp, bool exists)
    {
        Anchor storage a = _anchors[fabricTxId];
        return (a.commitment, a.blockNumber, a.evmTimestamp, a.exists);
    }

    function setRelayer(address relayer, bool allowed) external onlyOwner {
        if (relayer == address(0)) revert ZeroAddress();
        isRelayer[relayer] = allowed;
        emit RelayerSet(relayer, allowed);
    }

    function transferOwnership(address newOwner) external onlyOwner {
        if (newOwner == address(0)) revert ZeroAddress();
        emit OwnershipTransferred(owner, newOwner);
        owner = newOwner;
    }
}
