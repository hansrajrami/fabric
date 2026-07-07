import { ethers, network } from "hardhat";
import * as fs from "fs";
import * as path from "path";

// Deploys MSTAnchor. Configuration via env:
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
  const contract = await factory.deploy(allowlistEnabled, relayers);
  await contract.waitForDeployment();
  const address = await contract.getAddress();
  console.log(`MSTAnchor deployed at: ${address}`);

  const outDir = path.join(__dirname, "..", "deployments");
  fs.mkdirSync(outDir, { recursive: true });
  const record = {
    contract: "MSTAnchor",
    address,
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
