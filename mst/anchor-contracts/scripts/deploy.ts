import { ethers, network, upgrades } from "hardhat";
import * as fs from "fs";
import * as path from "path";

// Deploys MSTAnchor behind a TransparentUpgradeableProxy. The proxy address is
// the stable, channel-facing address; the implementation can be upgraded in place
// with scripts/upgrade.ts (the ProxyAdmin is owned by the deployer key — see
// README for the admin-key custody model). Configuration via env:
//   ANCHOR_ALLOWLIST_ENABLED=true|false   (default false)
//   ANCHOR_RELAYERS=0xabc...,0xdef...     (comma-separated initial relayers)
async function main() {
  const allowlistEnabled = (process.env.ANCHOR_ALLOWLIST_ENABLED ?? "false") === "true";
  const relayers = (process.env.ANCHOR_RELAYERS ?? "")
    .split(",")
    .map((a) => a.trim())
    .filter((a) => a.length > 0);

  const [deployer] = await ethers.getSigners();
  console.log(`network:   ${network.name}`);
  console.log(`deployer:  ${deployer.address}`);
  console.log(`allowlist: ${allowlistEnabled} relayers: [${relayers.join(", ")}]`);

  const factory = await ethers.getContractFactory("MSTAnchor");
  // initialize(bool,address[]) runs once, in the proxy's storage, with the deployer
  // as msg.sender (and therefore the contract owner).
  const proxy = await upgrades.deployProxy(factory, [allowlistEnabled, relayers], {
    kind: "transparent",
    initializer: "initialize",
  });
  await proxy.waitForDeployment();
  const address = await proxy.getAddress();
  const implAddress = await upgrades.erc1967.getImplementationAddress(address);
  const adminAddress = await upgrades.erc1967.getAdminAddress(address);
  console.log(`MSTAnchor proxy at:          ${address}`);
  console.log(`  implementation:            ${implAddress}`);
  console.log(`  proxy admin (upgrade key): ${adminAddress}`);

  const outDir = path.join(__dirname, "..", "deployments");
  fs.mkdirSync(outDir, { recursive: true });
  const record = {
    contract: "MSTAnchor",
    address, // the proxy — this is what a channel config points at
    implementation: implAddress,
    proxyAdmin: adminAddress,
    network: network.name,
    deployer: deployer.address,
    allowlistEnabled,
    relayers,
    deployedAt: new Date().toISOString(),
  };
  fs.writeFileSync(
    path.join(outDir, `${network.name}.json`),
    JSON.stringify(record, null, 2) + "\n"
  );
  console.log(`deployment record written to deployments/${network.name}.json`);
}

main().catch((err) => {
  console.error(err);
  process.exitCode = 1;
});
