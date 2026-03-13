- `[consensus]` Fix flaky `TestByzantinePrevoteEquivocation` by removing
  hardcoded 20s timeout in favor of the test's `-timeout` flag.
  ([#30](https://github.com/crypto-org-chain/cometbft/pull/30))
- `[state/indexer]` Fix psql test Docker port conflict by using explicit
  random host port binding.
  ([#30](https://github.com/crypto-org-chain/cometbft/pull/30))
