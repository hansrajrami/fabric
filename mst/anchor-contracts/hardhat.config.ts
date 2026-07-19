import { HardhatUserConfig, subtask } from "hardhat/config";
import { TASK_COMPILE_SOLIDITY_GET_SOLC_BUILD } from "hardhat/builtin-tasks/task-names";
import "@nomicfoundation/hardhat-toolbox";
import "@openzeppelin/hardhat-upgrades";

const SOLC_VERSION = "0.8.28";

// Use the solc build shipped in the npm `solc` package (same official emscripten build)
// instead of downloading from binaries.soliditylang.org, which some networks block.
subtask(TASK_COMPILE_SOLIDITY_GET_SOLC_BUILD, async (args: { solcVersion: string }, _hre, runSuper) => {
  if (args.solcVersion === SOLC_VERSION) {
    const compilerPath = require.resolve("solc/soljson.js");
    // eslint-disable-next-line @typescript-eslint/no-var-requires
    const longVersion: string = require("solc").version();
    return {
      compilerPath,
      isSolcJs: true,
      version: SOLC_VERSION,
      longVersion: longVersion.replace(/^(\d+\.\d+\.\d+\+commit\.[0-9a-f]+).*$/, "$1"),
    };
  }
  return runSuper(args);
});

// MST chain specifics are pure configuration: point MST_RPC_URL / MST_CHAIN_ID /
// MST_RELAYER_KEY at the real network and every script works unchanged.
const mstRpcUrl = process.env.MST_RPC_URL ?? "";
const mstChainId = process.env.MST_CHAIN_ID ? Number(process.env.MST_CHAIN_ID) : undefined;
const mstRelayerKey = process.env.MST_RELAYER_KEY;

const config: HardhatUserConfig = {
  solidity: {
    version: SOLC_VERSION,
    settings: {
      // High runs value optimizes runtime gas for the hot anchor() path over deploy cost.
      optimizer: { enabled: true, runs: 1_000_000 },
    },
  },
  networks: {
    localhost: {
      url: process.env.LOCAL_RPC_URL ?? "http://127.0.0.1:8545",
    },
    ...(mstRpcUrl
      ? {
          mst: {
            url: mstRpcUrl,
            chainId: mstChainId,
            accounts: mstRelayerKey ? [mstRelayerKey] : [],
          },
        }
      : {}),
  },
  mocha: { timeout: 60_000 },
};

export default config;
