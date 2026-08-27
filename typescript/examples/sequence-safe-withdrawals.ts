// Example 4 — sequence-safe concurrent withdrawal orchestration with
// SequenceGate (contracts/typescript-client-api.md §Examples).
//
// ⚠ SequenceGate is an IN-PROCESS helper only: no durability, no crash
// recovery, no cross-process serialization (the constructor warns about
// this). Production sequence management is the Go reference adapter's
// durable SequenceReservation path (data-model §6) — per-account database
// locks, one reservation per withdrawal, quarantine-not-release recovery.
//
// Run against a LOCAL/TEST chain with a funded key:
//
//   export SOVREN_LOCAL_CHAIN_RPC=http://127.0.0.1:26657
//   export SOVREN_DRILL_MNEMONIC="<funded test mnemonic>"   # UNSAFE_TEST_ONLY
//   npx tsx examples/sequence-safe-withdrawals.ts
//
// The demo fires five concurrent 0.1-SOVR withdrawals from one hot wallet;
// SequenceGate serializes them so each transaction carries a distinct,
// gap-free sequence.

import {
  SequenceGate,
  SovrenClient,
  assembleTx,
  buildMsgSend,
  deriveAddress,
  makeSignDocForTx,
  unsafeLocalSigner,
} from "../src/index.js";

async function main(): Promise<void> {
  const rpcUrl = process.env.SOVREN_LOCAL_CHAIN_RPC;
  const mnemonic = process.env.SOVREN_DRILL_MNEMONIC;
  if (rpcUrl === undefined || mnemonic === undefined) {
    console.error("set SOVREN_LOCAL_CHAIN_RPC and SOVREN_DRILL_MNEMONIC (see file header)");
    process.exit(1);
  }

  const client = await SovrenClient.connect(rpcUrl);
  const hot = await deriveAddress(mnemonic, "m/44'/118'/0'/0/0");
  const dest = await deriveAddress(mnemonic, "m/44'/118'/0'/0/1");
  const signer = unsafeLocalSigner(mnemonic, { unsafeTestOnly: true, networkType: "testnet" });
  const { block } = await client.block();
  const chainId = block.header.chainId;

  const gate = new SequenceGate(client);

  const sendOne = (n: number): Promise<string> =>
    gate.run(hot.bech32Address, async ({ accountNumber, sequence }) => {
      const unsigned = buildMsgSend({
        fromAddress: hot.bech32Address,
        toAddress: dest.bech32Address,
        amountBaseUnits: "100000",
        feeBaseUnits: "3250",
        gasLimit: 130000n,
        memo: `sequence-safe demo #${n}`,
      });
      // The signer boundary provides the pubkey; it is embedded in AuthInfo
      // before the sign-doc bytes are fixed (required by CheckTx).
      const { publicKeyCompressed } = await signer.getPublicKey({ keyRef: hot.bech32Address });
      const { signDocBytes, summary } = makeSignDocForTx(unsigned, {
        chainId,
        accountNumber,
        sequence,
        publicKeyCompressed,
      });
      const signed = await signer.sign({
        keyRef: hot.bech32Address,
        signMode: "SIGN_MODE_DIRECT",
        signDocBytes,
        summary,
      });
      const { txRawBytes, txHash } = assembleTx(unsigned, signed);
      const result = await client.broadcast(txRawBytes);
      if (!result.accepted) {
        throw new Error(`CheckTx rejected #${n} (seq ${sequence}): code ${result.code}: ${result.rawLog}`);
      }
      console.log(`#${n} seq=${sequence} accepted tx=${txHash}`);
      return txHash;
    });

  // Five concurrent withdrawals — without the gate these would race for the
  // same account sequence and all but one would be rejected.
  const hashes = await Promise.all([1, 2, 3, 4, 5].map(sendOne));

  console.log("waiting for inclusion...");
  for (const hash of hashes) {
    for (let i = 0; i < 30; i++) {
      const lookup = await client.tx(hash);
      if (lookup.found) {
        console.log(`included ${hash} height=${lookup.height} code=${lookup.code}`);
        break;
      }
      await new Promise((r) => setTimeout(r, 1000));
    }
  }
  client.disconnect();
}

main().catch((err: unknown) => {
  console.error(err);
  process.exit(1);
});
