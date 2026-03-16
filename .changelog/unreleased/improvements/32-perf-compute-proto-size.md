- `[mempool]` Replace proto marshaling with direct arithmetic in
  `ComputeProtoSizeForTxs`, eliminating per-tx allocations in the hot
  `ReapMaxBytesMaxGas` path for ~2x speedup.
  ([#32](https://github.com/crypto-org-chain/cometbft/pull/32))
