import { expect } from "chai";
import { ethers } from "hardhat";
import { readFileSync } from "fs";
import * as path from "path";

// The EVM leg of the cross-language vector gate (spec section 10): Solidity's
// own abi.encode + keccak256 must reproduce the abiEncoding and commitment of
// every commitment vector that the Go and TypeScript implementations agree on.
const vectors = JSON.parse(
  readFileSync(path.join(__dirname, "..", "..", "testvectors", "vectors.json"), "utf8")
);

describe("Commitment vectors on the EVM", () => {
  async function deployChecker() {
    const factory = await ethers.getContractFactory("CommitmentCheck");
    return factory.deploy();
  }

  it("domain tag matches keccak256 of the pinned preimage", async () => {
    const checker = await deployChecker();
    expect(await checker.domainTag()).to.equal(vectors.domainTag);
    expect(ethers.keccak256(ethers.toUtf8Bytes(vectors.domainTagPreimage))).to.equal(
      vectors.domainTag
    );
  });

  for (const v of vectors.commitments) {
    it(`recomputes ${v.id} byte-identically`, async () => {
      const checker = await deployChecker();
      const [encoded, commitment] = await checker.compute(
        vectors.schemaVersion,
        vectors.domainTag,
        v.fabricTxId,
        v.channelId,
        v.chaincodeId,
        v.blockNumber,
        v.timestamp,
        v.payloadHash
      );
      expect(encoded).to.equal(v.abiEncoding);
      expect(commitment).to.equal(v.commitment);
    });
  }
});
