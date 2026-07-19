// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

import {MSTAnchor} from "../MSTAnchor.sol";

/// @title  MSTAnchorV2 (test-only upgrade target)
/// @notice Storage-compatible successor to MSTAnchor: identical layout, adds a
///         `version()` getter. Used only to prove an in-place proxy upgrade preserves
///         the address and previously recorded anchors. Not a production contract.
contract MSTAnchorV2 is MSTAnchor {
    function version() external pure returns (uint256) {
        return 2;
    }
}
