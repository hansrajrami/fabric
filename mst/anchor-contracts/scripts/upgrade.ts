import { ethers, network, upgrades } from "hardhat";
import * as fs from "fs";
import * as path from "path";

// Upgrades an existing MSTAnchor proxy to a new implementation IN PLACE. The proxy
// address (and therefore the whole anchor history) is preserved. Configuration:
//   PROXY_ADDRESS=0x...           the proxy to upgrade (default: deployments/<network>.json)
//   NEW_CONTRACT=MSTAnchor        the new implementation contract name (default MSTAnchor)
//
// The caller must control the proxy admin (the deployer key by default). This is the
// operation that makes recorded anchors governance-mutable — treat the admin key
// accordingly (see README).
async function main() {
  const outFile = path.join(__dirname, "..", "deployments", `${network.name}.json`);
  let proxyAddress = process.env.PROXY_ADDRESS ?? "";
  if (!proxyAddress && fs.existsSync(outFile)) {
    proxyAddress = JSON.parse(fs.readFileSync(outFile, "utf8")).address;
  }
  if (!proxyAddress) {
    throw new Error("no proxy address: set PROXY_ADDRESS or run deploy first");
  }
  const contractName = process.env.NEW_CONTRACT ?? "MSTAnchor";

  const [deployer] = await ethers.getSigners();
  console.log(`network:   ${network.name}`);
  console.log(`upgrader:  ${deployer.address}`);
  console.log(`proxy:     ${proxyAddress}`);
  console.log(`new impl:  ${contractName}`);

  const factory = await ethers.getContractFactory(contractName);
  // upgradeProxy runs the storage-layout compatibility checks before switching.
  const upgraded = await upgrades.upgradeProxy(proxyAddress, factory);
  await upgraded.waitForDeployment();
  const implAddress = await upgrades.erc1967.getImplementationAddress(proxyAddress);
  console.log(`upgraded. new implementation: ${implAddress}`);

  if (fs.existsSync(outFile)) {
    const record = JSON.parse(fs.readFileSync(outFile, "utf8"));
    record.implementation = implAddress;
    record.upgradedAt = new Date().toISOString();
    fs.writeFileSync(outFile, JSON.stringify(record, null, 2) + "\n");
    console.log(`deployment record updated: deployments/${network.name}.json`);
  }
}

main().catch((err) => {
  console.error(err);
  process.exitCode = 1;
});
