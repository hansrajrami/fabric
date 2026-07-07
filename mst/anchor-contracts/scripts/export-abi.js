// Copies the compiled MSTAnchor ABI to abi/MSTAnchor.json (checked in), so the Go
// relayer's calldata packing can be unit-tested against the exact compiled ABI
// without needing the Hardhat toolchain.
const fs = require("fs");
const path = require("path");

const artifact = path.join(
  __dirname,
  "..",
  "artifacts",
  "contracts",
  "MSTAnchor.sol",
  "MSTAnchor.json"
);
const outDir = path.join(__dirname, "..", "abi");

const parsed = JSON.parse(fs.readFileSync(artifact, "utf8"));
fs.mkdirSync(outDir, { recursive: true });
fs.writeFileSync(
  path.join(outDir, "MSTAnchor.json"),
  JSON.stringify({ contractName: parsed.contractName, abi: parsed.abi }, null, 2) + "\n"
);
console.log("ABI exported to abi/MSTAnchor.json");
