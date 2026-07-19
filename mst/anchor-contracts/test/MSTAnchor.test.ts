import { expect } from "chai";
import { ethers, upgrades } from "hardhat";
import { loadFixture } from "@nomicfoundation/hardhat-toolbox/network-helpers";
import type { MSTAnchor } from "../typechain-types";

const id = (s: string) => ethers.keccak256(ethers.toUtf8Bytes(s));

// Deploy MSTAnchor behind a TransparentUpgradeableProxy (its real deployment shape).
// initialize(bool,address[]) runs once with the deployer as owner.
async function deployProxy(allowlistEnabled: boolean, relayers: string[]): Promise<MSTAnchor> {
  const factory = await ethers.getContractFactory("MSTAnchor");
  const proxy = await upgrades.deployProxy(factory, [allowlistEnabled, relayers], {
    kind: "transparent",
    initializer: "initialize",
  });
  await proxy.waitForDeployment();
  return proxy as unknown as MSTAnchor;
}

describe("MSTAnchor", () => {
  async function deployOpen() {
    const [deployer, other] = await ethers.getSigners();
    const contract = await deployProxy(false, []);
    return { contract, deployer, other };
  }

  async function deployAllowlisted() {
    const [deployer, relayer, stranger] = await ethers.getSigners();
    const contract = await deployProxy(true, [relayer.address]);
    return { contract, deployer, relayer, stranger };
  }

  describe("anchoring", () => {
    it("stores the first anchor and emits Anchored with the block timestamp", async () => {
      const { contract } = await loadFixture(deployOpen);
      const txId = id("tx-1");
      const commitment = id("commitment-1");

      const tx = await contract.anchor(txId, commitment, 42n);
      const receipt = await tx.wait();
      const block = await ethers.provider.getBlock(receipt!.blockNumber);

      await expect(tx)
        .to.emit(contract, "Anchored")
        .withArgs(txId, commitment, 42n, BigInt(block!.timestamp));

      const [gotCommitment, gotBlock, gotTs, exists] = await contract.getAnchor(txId);
      expect(gotCommitment).to.equal(commitment);
      expect(gotBlock).to.equal(42n);
      expect(gotTs).to.equal(BigInt(block!.timestamp));
      expect(exists).to.equal(true);
    });

    it("duplicate anchor is a quiet no-op: no revert, no overwrite, no event", async () => {
      const { contract, other } = await loadFixture(deployOpen);
      const txId = id("tx-dup");
      const original = id("original");
      const attacker = id("attacker-overwrite");

      await (await contract.anchor(txId, original, 7n)).wait();

      // Different sender, different commitment, different block: must not change anything.
      const dupTx = await contract.connect(other).anchor(txId, attacker, 9999n);
      const dupReceipt = await dupTx.wait();
      expect(dupReceipt!.logs.length).to.equal(0);

      const [gotCommitment, gotBlock, , exists] = await contract.getAnchor(txId);
      expect(gotCommitment).to.equal(original);
      expect(gotBlock).to.equal(7n);
      expect(exists).to.equal(true);
    });

    it("getAnchor for an unknown id reports exists=false and zero values", async () => {
      const { contract } = await loadFixture(deployOpen);
      const [commitment, blockNumber, ts, exists] = await contract.getAnchor(id("never"));
      expect(commitment).to.equal(ethers.ZeroHash);
      expect(blockNumber).to.equal(0n);
      expect(ts).to.equal(0n);
      expect(exists).to.equal(false);
    });

    it("distinct ids are independent records", async () => {
      const { contract } = await loadFixture(deployOpen);
      await (await contract.anchor(id("a"), id("ca"), 1n)).wait();
      await (await contract.anchor(id("b"), id("cb"), 2n)).wait();
      const [ca, , , existsA] = await contract.getAnchor(id("a"));
      const [cb, , , existsB] = await contract.getAnchor(id("b"));
      expect(ca).to.equal(id("ca"));
      expect(cb).to.equal(id("cb"));
      expect(existsA).to.equal(true);
      expect(existsB).to.equal(true);
    });
  });

  describe("anchorBatch", () => {
    it("anchors new ids and quietly skips already-anchored ones", async () => {
      const { contract } = await loadFixture(deployOpen);
      const pre = id("pre-existing");
      const preCommitment = id("pre-commitment");
      await (await contract.anchor(pre, preCommitment, 1n)).wait();

      const ids = [pre, id("new-1"), id("new-2")];
      const commitments = [id("evil-overwrite"), id("c1"), id("c2")];
      const tx = await contract.anchorBatch(ids, commitments, [100n, 101n, 102n]);
      const receipt = await tx.wait();

      // Exactly two Anchored events: the duplicate emitted nothing.
      expect(receipt!.logs.length).to.equal(2);

      const [gotPre] = await contract.getAnchor(pre);
      expect(gotPre).to.equal(preCommitment);
      const [c1, b1, , e1] = await contract.getAnchor(id("new-1"));
      expect(c1).to.equal(id("c1"));
      expect(b1).to.equal(101n);
      expect(e1).to.equal(true);
    });

    it("reverts on mismatched array lengths", async () => {
      const { contract } = await loadFixture(deployOpen);
      await expect(
        contract.anchorBatch([id("x")], [id("c"), id("d")], [1n])
      ).to.be.revertedWith("length mismatch");
    });
  });

  describe("relayer allowlist", () => {
    it("blocks non-relayers when enabled and allows listed relayers", async () => {
      const { contract, relayer, stranger } = await loadFixture(deployAllowlisted);

      await expect(
        contract.connect(stranger).anchor(id("t"), id("c"), 1n)
      ).to.be.revertedWithCustomError(contract, "NotRelayer");
      await expect(
        contract.connect(stranger).anchorBatch([id("t")], [id("c")], [1n])
      ).to.be.revertedWithCustomError(contract, "NotRelayer");

      await expect(contract.connect(relayer).anchor(id("t"), id("c"), 1n)).to.emit(
        contract,
        "Anchored"
      );
    });

    it("owner manages the allowlist; non-owner cannot", async () => {
      const { contract, deployer, relayer, stranger } = await loadFixture(deployAllowlisted);

      await expect(contract.connect(deployer).setRelayer(stranger.address, true))
        .to.emit(contract, "RelayerSet")
        .withArgs(stranger.address, true);
      await expect(contract.connect(stranger).anchor(id("s"), id("c"), 1n)).to.emit(
        contract,
        "Anchored"
      );

      await expect(contract.connect(deployer).setRelayer(relayer.address, false))
        .to.emit(contract, "RelayerSet")
        .withArgs(relayer.address, false);
      await expect(
        contract.connect(relayer).anchor(id("r2"), id("c"), 1n)
      ).to.be.revertedWithCustomError(contract, "NotRelayer");

      await expect(
        contract.connect(stranger).setRelayer(stranger.address, true)
      ).to.be.revertedWithCustomError(contract, "NotOwner");
      await expect(
        contract.connect(deployer).setRelayer(ethers.ZeroAddress, true)
      ).to.be.revertedWithCustomError(contract, "ZeroAddress");
    });

    it("anyone can anchor when the allowlist is disabled", async () => {
      const { contract, other } = await loadFixture(deployOpen);
      await expect(contract.connect(other).anchor(id("t"), id("c"), 1n)).to.emit(
        contract,
        "Anchored"
      );
    });

    it("initializer rejects a zero relayer address", async () => {
      const factory = await ethers.getContractFactory("MSTAnchor");
      await expect(
        upgrades.deployProxy(factory, [true, [ethers.ZeroAddress]], {
          kind: "transparent",
          initializer: "initialize",
        })
      ).to.be.revertedWithCustomError(factory, "ZeroAddress");
    });
  });

  describe("anchorRoot (Merkle batches)", () => {
    it("stores the first root and emits RootAnchored", async () => {
      const { contract } = await loadFixture(deployOpen);
      const root = id("batch-root-1");
      const tx = await contract.anchorRoot(root, 20n);
      const receipt = await tx.wait();
      const block = await ethers.provider.getBlock(receipt!.blockNumber);
      await expect(tx)
        .to.emit(contract, "RootAnchored")
        .withArgs(root, 20n, BigInt(block!.timestamp));

      const [leafCount, ts, exists] = await contract.getRoot(root);
      expect(leafCount).to.equal(20n);
      expect(ts).to.equal(BigInt(block!.timestamp));
      expect(exists).to.equal(true);
    });

    it("duplicate root is a quiet no-op: no revert, no overwrite, no event", async () => {
      const { contract, other } = await loadFixture(deployOpen);
      const root = id("batch-root-dup");
      await (await contract.anchorRoot(root, 5n)).wait();
      const dup = await contract.connect(other).anchorRoot(root, 9999n);
      const dupReceipt = await dup.wait();
      expect(dupReceipt!.logs.length).to.equal(0);
      const [leafCount, , exists] = await contract.getRoot(root);
      expect(leafCount).to.equal(5n);
      expect(exists).to.equal(true);
    });

    it("unknown root reports exists=false", async () => {
      const { contract } = await loadFixture(deployOpen);
      const [leafCount, ts, exists] = await contract.getRoot(id("never"));
      expect(leafCount).to.equal(0n);
      expect(ts).to.equal(0n);
      expect(exists).to.equal(false);
    });

    it("respects the relayer allowlist", async () => {
      const { contract, relayer, stranger } = await loadFixture(deployAllowlisted);
      await expect(
        contract.connect(stranger).anchorRoot(id("r"), 1n)
      ).to.be.revertedWithCustomError(contract, "NotRelayer");
      await expect(contract.connect(relayer).anchorRoot(id("r"), 1n)).to.emit(
        contract,
        "RootAnchored"
      );
    });
  });

  describe("ownership", () => {
    it("transfers ownership and rejects zero address / non-owner", async () => {
      const { contract, deployer, other } = await loadFixture(deployOpen);

      await expect(
        contract.connect(other).transferOwnership(other.address)
      ).to.be.revertedWithCustomError(contract, "NotOwner");
      await expect(
        contract.connect(deployer).transferOwnership(ethers.ZeroAddress)
      ).to.be.revertedWithCustomError(contract, "ZeroAddress");

      await expect(contract.connect(deployer).transferOwnership(other.address))
        .to.emit(contract, "OwnershipTransferred")
        .withArgs(deployer.address, other.address);
      expect(await contract.owner()).to.equal(other.address);
    });
  });

  describe("upgradeability", () => {
    it("upgrades in place: same address, prior anchors preserved, new logic live", async () => {
      const [deployer] = await ethers.getSigners();
      const proxy = await deployProxy(false, []);
      const addrBefore = await proxy.getAddress();

      // Record an anchor on v1.
      const txId = id("pre-upgrade");
      const commitment = id("c-pre");
      await (await proxy.anchor(txId, commitment, 5n)).wait();

      // Upgrade the implementation in place (storage-layout checked by the plugin).
      const v2Factory = await ethers.getContractFactory("MSTAnchorV2");
      const upgraded = await upgrades.upgradeProxy(addrBefore, v2Factory);
      await upgraded.waitForDeployment();

      // The channel-facing address is unchanged — no history split.
      expect(await upgraded.getAddress()).to.equal(addrBefore);

      // The anchor recorded before the upgrade is still readable.
      const [gotCommitment, gotBlock, , exists] = await upgraded.getAnchor(txId);
      expect(gotCommitment).to.equal(commitment);
      expect(gotBlock).to.equal(5n);
      expect(exists).to.equal(true);

      // Owner (initialized state) survives the upgrade.
      expect(await upgraded.owner()).to.equal(deployer.address);

      // New implementation logic is live at the same address.
      const v2 = await ethers.getContractAt("MSTAnchorV2", addrBefore);
      expect(await v2.version()).to.equal(2n);

      // Anchoring still works post-upgrade.
      await expect(upgraded.anchor(id("post-upgrade"), id("c-post"), 6n)).to.emit(
        upgraded,
        "Anchored"
      );
    });
  });

  describe("gas", () => {
    it("keeps single-anchor gas under budget", async () => {
      const { contract } = await loadFixture(deployOpen);
      const tx = await contract.anchor(id("gas"), id("c"), 1n);
      const receipt = await tx.wait();
      // 2 cold storage slots + event: expect well under 100k.
      expect(receipt!.gasUsed).to.be.lessThan(100_000n);
    });

    it("duplicate anchor costs only a warm read", async () => {
      const { contract } = await loadFixture(deployOpen);
      await (await contract.anchor(id("gas2"), id("c"), 1n)).wait();
      const dup = await (await contract.anchor(id("gas2"), id("c"), 1n)).wait();
      expect(dup!.gasUsed).to.be.lessThan(35_000n);
    });
  });
});
