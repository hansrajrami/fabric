// SPDX-License-Identifier: MIT
pragma solidity 0.8.28;

// Re-export OpenZeppelin's TransparentUpgradeableProxy so Hardhat compiles it into
// artifacts/. This gives the Go integration test (mst/relay/evm) the proxy creation
// bytecode it needs to deploy the proxy directly, without pulling in the hardhat-upgrades
// plugin from Go. The TS side (tests, deploy) uses upgrades.deployProxy instead.
import {TransparentUpgradeableProxy} from "@openzeppelin/contracts/proxy/transparent/TransparentUpgradeableProxy.sol";
