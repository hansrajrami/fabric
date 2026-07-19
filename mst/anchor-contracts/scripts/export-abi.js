// Copies compiled ABIs+bytecode to abi/ (checked in), so the Go relayer's calldata
// packing can be unit-tested against the exact compiled ABI, and the Go integration
// test can deploy the implementation + TransparentUpgradeableProxy, without needing the
// Hardhat toolchain at test time.
const fs = require("fs");
const path = require("path");

const artifactsDir = path.join(__dirname, "..", "artifacts");
const outDir = path.join(__dirname, "..", "abi");
fs.mkdirSync(outDir, { recursive: true });

function exportArtifact(artifactPath, outName) {
  const parsed = JSON.parse(fs.readFileSync(artifactPath, "utf8"));
  fs.writeFileSync(
    path.join(outDir, outName),
    JSON.stringify(
      { contractName: parsed.contractName, abi: parsed.abi, bytecode: parsed.bytecode },
      null,
      2
    ) + "\n"
  );
  console.log(`exported abi/${outName}`);
}

exportArtifact(
  path.join(artifactsDir, "contracts", "MSTAnchor.sol", "MSTAnchor.json"),
  "MSTAnchor.json"
);
// The proxy bytecode lets the Go integration test deploy the real OZ
// TransparentUpgradeableProxy in front of the implementation.
exportArtifact(
  path.join(
    artifactsDir,
    "@openzeppelin",
    "contracts",
    "proxy",
    "transparent",
    "TransparentUpgradeableProxy.sol",
    "TransparentUpgradeableProxy.json"
  ),
  "TransparentUpgradeableProxy.json"
);
