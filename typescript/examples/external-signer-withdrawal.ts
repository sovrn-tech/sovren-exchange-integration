// Example 3 — build → externally sign → broadcast → confirm a withdrawal
// (contracts/typescript-client-api.md §Examples). Testnet-ready.
//
// The transaction is built and broadcast WITHOUT the key ever touching this
// process's tx pipeline: signing crosses the TransactionSigner boundary
// (contracts/signer-interface.md). Here the boundary is fed by
// fromOfflineDirectSigner over a CosmJS wallet as a stand-in for the
// exchange's real signing system (HSM/MPC/air-gap service) — swap
// `makeSigner` for your own TransactionSigner and nothing else changes.
//
// Requirements: a running test network and a FUNDED key (≥ 1.1 SOVR):
//
//   export SOVREN_LOCAL_CHAIN_RPC=http://127.0.0.1:26657   # or a testnet RPC
//   export SOVREN_DRILL_MNEMONIC="<funded test mnemonic>"  # UNSAFE_TEST_ONLY
//   npx tsx examples/external-signer-withdrawal.ts sovr1...destination 1000000
//
// Never use a production mnemonic here; production keys live only behind
// the exchange's own signer service.

import { stringToPath } from "@cosmjs/crypto";
import { DirectSecp256k1HdWallet } from "@cosmjs/proto-signing";

import {
  DEFAULT_HD_PATH,
  SovrenClient,
  type TransactionSigner,
  assembleTx,
  buildMsgSend,
  defaultProhibitedModuleAccounts,
  deriveAddress,
  fromOfflineDirectSigner,
  makeSignDocForTx,
  validateAccountAddress,
} from "../src/index.js";

async function makeSigner(mnemonic: string): Promise<TransactionSigner> {
  // Stand-in for the exchange's signing system: any OfflineDirectSigner
  // bridges into the kit's TransactionSigner boundary.
  const wallet = await DirectSecp256k1HdWallet.fromMnemonic(mnemonic, {
    prefix: "sovr",
    hdPaths: [stringToPath(DEFAULT_HD_PATH)],
  });
  return fromOfflineDirectSigner(wallet);
}

async function main(): Promise<void> {
  const rpcUrl = process.env.SOVREN_LOCAL_CHAIN_RPC;
  const mnemonic = process.env.SOVREN_DRILL_MNEMONIC;
  const [destination, amountBaseUnits = "1000000"] = process.argv.slice(2);
  if (rpcUrl === undefined || mnemonic === undefined || destination === undefined) {
    console.error("usage: SOVREN_LOCAL_CHAIN_RPC=... SOVREN_DRILL_MNEMONIC=... tsx external-signer-withdrawal.ts <sovr1-destination> [amount-usovr]");
    process.exit(1);
  }

  // FR-032 checklist head: strict destination validation before anything else,
  // INCLUDING the module-account blocklist (parity with the Go kit). Bare bech32
  // validation accepts these addresses; the blocklist prevents two distinct
  // failures — a send to a blocked account (e.g. fee_collector) is rejected
  // on-chain (a stuck/failed withdrawal), while a send to the unblocked `gov`
  // sink SUCCEEDS and strands the funds. Reject both before signing.
  const check = validateAccountAddress(destination, { prohibited: defaultProhibitedModuleAccounts() });
  if (!check.valid) {
    console.error(`destination rejected: ${check.errorCode}`);
    process.exit(1);
  }

  const client = await SovrenClient.connect(rpcUrl);
  const hot = await deriveAddress(mnemonic, DEFAULT_HD_PATH);
  const signer = await makeSigner(mnemonic);
  const { block } = await client.block();
  const chainId = block.header.chainId;

  const account = await client.account(hot.bech32Address);
  const balance = await client.balance(hot.bech32Address);
  console.log(`hot wallet ${hot.bech32Address} balance=${balance.amount}usovr sequence=${account.sequence}`);

  // 1. Build (one MsgSend per transaction — FR-036).
  const unsigned = buildMsgSend({
    fromAddress: hot.bech32Address,
    toAddress: check.normalizedAddress!,
    amountBaseUnits,
    feeBaseUnits: "3250",
    gasLimit: 130000n,
    memo: "external-signer demo",
  });
  // The signer boundary provides the sender pubkey; it is embedded in
  // AuthInfo before the sign-doc bytes are fixed (required by CheckTx).
  const { publicKeyCompressed } = await signer.getPublicKey({ keyRef: hot.bech32Address });
  const { signDocBytes, summary } = makeSignDocForTx(unsigned, {
    chainId,
    accountNumber: account.accountNumber,
    sequence: account.sequence,
    publicKeyCompressed,
  });
  console.log("summary (derived from the sign-doc bytes):", summary);

  // 2. Externally sign. The signer re-derives the summary from the bytes
  //    and refuses on any mismatch (SUMMARY_MISMATCH).
  const signed = await signer.sign({
    keyRef: hot.bech32Address,
    signMode: "SIGN_MODE_DIRECT",
    signDocBytes,
    summary,
  });

  // 3. Assemble + broadcast.
  const { txRawBytes, txHash } = assembleTx(unsigned, signed);
  const result = await client.broadcast(txRawBytes);
  if (!result.accepted) {
    console.error(`CheckTx rejected: code ${result.code} (${result.codespace}): ${result.rawLog}`);
    process.exit(1);
  }
  console.log(`broadcast accepted: ${txHash}`);

  // 4. Confirm: poll for inclusion, then report the EXECUTION result —
  //    inclusion with code != 0 means the transfer did NOT happen.
  for (let i = 0; i < 60; i++) {
    const lookup = await client.tx(txHash);
    if (lookup.found) {
      if (lookup.code === 0) {
        console.log(`CONFIRMED at height ${lookup.height}`);
      } else {
        console.error(`FAILED on-chain: code ${lookup.code}: ${lookup.rawLog}`);
        process.exitCode = 1;
      }
      client.disconnect();
      return;
    }
    await new Promise((r) => setTimeout(r, 1000));
  }
  console.error(`tx ${txHash} not found after 60s — do NOT re-sign; search and reconcile first (FR-035)`);
  client.disconnect();
  process.exit(1);
}

main().catch((err: unknown) => {
  console.error(err);
  process.exit(1);
});
