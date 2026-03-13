- `[consensus]` Skip fsync for unsigned internal WAL messages (block parts),
  reducing per-round I/O from 6+N fsyncs to 6.
  ([#28](https://github.com/crypto-org-chain/cometbft/pull/28))
