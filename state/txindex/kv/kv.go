package kv

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"

	"github.com/cometbft/cometbft/libs/log"

	"github.com/cosmos/gogoproto/proto"

	dbm "github.com/cometbft/cometbft-db"

	abci "github.com/cometbft/cometbft/abci/types"
	idxutil "github.com/cometbft/cometbft/internal/indexer"
	"github.com/cometbft/cometbft/libs/pubsub/query"
	"github.com/cometbft/cometbft/libs/pubsub/query/syntax"
	"github.com/cometbft/cometbft/state/indexer"
	"github.com/cometbft/cometbft/state/txindex"
	"github.com/cometbft/cometbft/types"
)

const (
	tagKeySeparator     = "/"
	tagKeySeparatorRune = '/'
	eventSeqSeparator   = "$es$"
)

var (
	_ txindex.TxIndexer    = (*TxIndex)(nil)
	_ txindex.PageSearcher = (*TxIndex)(nil)
)

// TxIndex is the simplest possible indexer, backed by key-value storage (levelDB).
type TxIndex struct {
	store dbm.DB
	// Number the events in the event list
	eventSeq int64

	log log.Logger
}

// NewTxIndex creates new KV indexer.
func NewTxIndex(store dbm.DB) *TxIndex {
	return &TxIndex{
		store: store,
	}
}

func (txi *TxIndex) SetLogger(l log.Logger) {
	txi.log = l
}

// Get gets transaction from the TxIndex storage and returns it or nil if the
// transaction is not found.
func (txi *TxIndex) Get(hash []byte) (*abci.TxResult, error) {
	if len(hash) == 0 {
		return nil, txindex.ErrorEmptyHash
	}

	rawBytes, err := txi.store.Get(hash)
	if err != nil {
		panic(err)
	}
	if rawBytes == nil {
		return nil, nil
	}

	txResult := new(abci.TxResult)
	err = proto.Unmarshal(rawBytes, txResult)
	if err != nil {
		return nil, fmt.Errorf("error reading TxResult: %v", err)
	}

	return txResult, nil
}

// AddBatch indexes a batch of transactions using the given list of events. Each
// key that indexed from the tx's events is a composite of the event type and
// the respective attribute's key delimited by a "." (eg. "account.number").
// Any event with an empty type is not indexed.
func (txi *TxIndex) AddBatch(b *txindex.Batch) error {
	storeBatch := txi.store.NewBatch()
	defer storeBatch.Close()

	for _, result := range b.Ops {
		hash := types.Tx(result.Tx).Hash()

		// index tx by events
		err := txi.indexEvents(result, hash, storeBatch)
		if err != nil {
			return err
		}

		// index by height (always)
		err = storeBatch.Set(keyForHeight(result), hash)
		if err != nil {
			return err
		}

		rawBytes, err := proto.Marshal(result)
		if err != nil {
			return err
		}
		// index by hash (always)
		err = storeBatch.Set(hash, rawBytes)
		if err != nil {
			return err
		}
	}

	return storeBatch.WriteSync()
}

// Index indexes a single transaction using the given list of events. Each key
// that indexed from the tx's events is a composite of the event type and the
// respective attribute's key delimited by a "." (eg. "account.number").
// Any event with an empty type is not indexed.
//
// If a transaction is indexed with the same hash as a previous transaction, it will
// be overwritten unless the tx result was NOT OK and the prior result was OK i.e.
// more transactions that successfully executed overwrite transactions that failed
// or successful yet older transactions.
func (txi *TxIndex) Index(result *abci.TxResult) error {
	b := txi.store.NewBatch()
	defer b.Close()

	hash := types.Tx(result.Tx).Hash()

	if !result.Result.IsOK() {
		oldResult, err := txi.Get(hash)
		if err != nil {
			return err
		}

		// if the new transaction failed and it's already indexed in an older block and was successful
		// we skip it as we want users to get the older successful transaction when they query.
		if oldResult != nil && oldResult.Result.Code == abci.CodeTypeOK {
			return nil
		}
	}

	// index tx by events
	err := txi.indexEvents(result, hash, b)
	if err != nil {
		return err
	}

	// index by height (always)
	err = b.Set(keyForHeight(result), hash)
	if err != nil {
		return err
	}

	rawBytes, err := proto.Marshal(result)
	if err != nil {
		return err
	}
	// index by hash (always)
	err = b.Set(hash, rawBytes)
	if err != nil {
		return err
	}

	return b.WriteSync()
}

func (txi *TxIndex) indexEvents(result *abci.TxResult, hash []byte, store dbm.Batch) error {
	for _, event := range result.Result.Events {
		txi.eventSeq++
		// only index events with a non-empty type
		if len(event.Type) == 0 {
			continue
		}

		for _, attr := range event.Attributes {
			if len(attr.Key) == 0 {
				continue
			}

			// index if `index: true` is set
			compositeTag := fmt.Sprintf("%s.%s", event.Type, attr.Key)
			// ensure event does not conflict with a reserved prefix key
			if compositeTag == types.TxHashKey || compositeTag == types.TxHeightKey {
				return fmt.Errorf("event type and attribute key \"%s\" is reserved; please use a different key", compositeTag)
			}
			if attr.GetIndex() {
				err := store.Set(keyForEvent(compositeTag, attr.Value, result, txi.eventSeq), hash)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

// Search performs a search using the given query.
//
// It breaks the query into conditions (like "tx.height > 5"). For each
// condition, it queries the DB index. One special use cases here: (1) if
// "tx.hash" is found, it returns tx result for it (2) for range queries it is
// better for the client to provide both lower and upper bounds, so we are not
// performing a full scan. Results from querying indexes are then intersected
// and returned to the caller, in no particular order.
//
// Search will exit early and return any result fetched so far,
// when a message is received on the context chan.
func (txi *TxIndex) Search(ctx context.Context, q *query.Query) ([]*abci.TxResult, error) {
	results, _, err := txi.search(ctx, q, nil)
	return results, err
}

// SearchPage works like Search but orders and paginates matches before any tx
// is loaded, so only the requested page is read from the store. It also
// returns the total number of matches.
func (txi *TxIndex) SearchPage(
	ctx context.Context,
	q *query.Query,
	pagSettings txindex.Pagination,
) ([]*abci.TxResult, int, error) {
	return txi.search(ctx, q, &pagSettings)
}

// search returns all matches unordered when pag is nil.
func (txi *TxIndex) search(
	ctx context.Context,
	q *query.Query,
	pag *txindex.Pagination,
) ([]*abci.TxResult, int, error) {
	select {
	case <-ctx.Done():
		return make([]*abci.TxResult, 0), 0, nil

	default:
	}

	var hashesInitialized bool
	filteredHashes := make(map[string]txInfo)

	// get a list of conditions (like "tx.height > 5")
	conditions := q.Syntax()

	// if there is a hash condition, return the result immediately
	hash, ok, err := lookForHash(conditions)
	if err != nil {
		return nil, 0, fmt.Errorf("error during searching for a hash in the query: %w", err)
	} else if ok {
		res, err := txi.Get(hash)
		switch {
		case err != nil:
			return []*abci.TxResult{}, 0, fmt.Errorf("error while retrieving the result: %w", err)
		case res == nil:
			return []*abci.TxResult{}, 0, nil
		default:
			results := []*abci.TxResult{res}
			if pag != nil {
				results = txindex.Paginate(results, pag.Page, pag.PerPage)
			}
			return results, 1, nil
		}
	}

	// conditions to skip because they're handled before "everything else"
	skipIndexes := make([]int, 0)
	var heightInfo HeightInfo

	// If we are not matching events and tx.height = 3 occurs more than once, the later value will
	// overwrite the first one.
	conditions, heightInfo = dedupHeight(conditions)

	if !heightInfo.onlyHeightEq {
		skipIndexes = append(skipIndexes, heightInfo.heightEqIdx)
	}

	// extract ranges
	// if both upper and lower bounds exist, it's better to get them in order not
	// no iterate over kvs that are not within range.
	ranges, rangeIndexes, heightRange := indexer.LookForRangesWithHeight(conditions)
	heightInfo.heightRange = heightRange
	if len(ranges) > 0 {
		skipIndexes = append(skipIndexes, rangeIndexes...)

		for _, qr := range ranges {

			// If we have a query range over height and want to still look for
			// specific event values we do not want to simply return all
			// transactios in this height range. We remember the height range info
			// and pass it on to match() to take into account when processing events.
			if qr.Key == types.TxHeightKey && !heightInfo.onlyHeightRange {
				continue
			}
			if !hashesInitialized {
				filteredHashes = txi.matchRange(ctx, qr, startKey(qr.Key), filteredHashes, true, heightInfo)
				hashesInitialized = true

				// Ignore any remaining conditions if the first condition resulted
				// in no matches (assuming implicit AND operand).
				if len(filteredHashes) == 0 {
					break
				}
			} else {
				filteredHashes = txi.matchRange(ctx, qr, startKey(qr.Key), filteredHashes, false, heightInfo)
			}
		}
	}

	// if there is a height condition ("tx.height=3"), extract it

	// for all other conditions
	for i, c := range conditions {
		if intInSlice(i, skipIndexes) {
			continue
		}

		if !hashesInitialized {
			filteredHashes = txi.match(ctx, c, startKeyForCondition(c, heightInfo.height), filteredHashes, true, heightInfo)
			hashesInitialized = true

			// Ignore any remaining conditions if the first condition resulted
			// in no matches (assuming implicit AND operand).
			if len(filteredHashes) == 0 {
				break
			}
		} else {
			filteredHashes = txi.match(ctx, c, startKeyForCondition(c, heightInfo.height), filteredHashes, false, heightInfo)
		}
	}

	txs := uniqueTxInfos(filteredHashes)
	totalCount := len(txs)
	if pag != nil {
		sortTxInfos(txs, pag.OrderDesc)
		txs = txindex.Paginate(txs, pag.Page, pag.PerPage)
	}

	results := make([]*abci.TxResult, 0, len(txs))
RESULTS_LOOP:
	for _, info := range txs {
		res, err := txi.Get([]byte(info.hash))
		if err != nil {
			return nil, 0, fmt.Errorf("failed to get Tx{%X}: %w", info.hash, err)
		}
		if res == nil {
			// The event key outlived the tx it points to. It is still part of
			// totalCount, as that is known only by loading every match.
			continue
		}
		results = append(results, res)

		// Potentially exit early.
		select {
		case <-ctx.Done():
			break RESULTS_LOOP
		default:
		}
	}

	return results, totalCount, nil
}

// txInfo identifies a matched tx by the position recorded in its event key.
// hash shares memory with the key of the map it is stored in.
type txInfo struct {
	hash   string
	height int64
	index  uint32
}

// after reports whether a is at a higher position than b.
func (a txInfo) after(b txInfo) bool {
	return a.height > b.height || (a.height == b.height && a.index > b.index)
}

func sortTxInfos(txs []txInfo, desc bool) {
	sort.Slice(txs, func(i, j int) bool {
		a, b := txs[i], txs[j]
		if desc {
			a, b = b, a
		}
		if a.height != b.height {
			return a.height < b.height
		}
		if a.index != b.index {
			return a.index < b.index
		}
		return a.hash < b.hash
	})
}

// uniqueTxInfos returns one entry per matched tx. filteredHashes holds one
// entry per matching event key, and the keys of a tx normally share its height
// and index. A tx indexed again at another position (see Index) keeps its old
// keys though; for such a tx the highest matched position is kept. That can
// still be a stale one, e.g. when only the old keys match the query, in which
// case the tx is ordered by its old position rather than the stored result's.
func uniqueTxInfos(filteredHashes map[string]txInfo) []txInfo {
	byHash := make(map[string]int, len(filteredHashes))
	txs := make([]txInfo, 0, len(filteredHashes))
	for _, info := range filteredHashes {
		i, ok := byHash[info.hash]
		switch {
		case !ok:
			byHash[info.hash] = len(txs)
			txs = append(txs, info)
		case info.after(txs[i]):
			txs[i] = info
		}
	}
	return txs
}

func lookForHash(conditions []syntax.Condition) (hash []byte, ok bool, err error) {
	for _, c := range conditions {
		if c.Tag == types.TxHashKey {
			decoded, err := hex.DecodeString(c.Arg.Value())
			return decoded, true, err
		}
	}
	return
}

func (txi *TxIndex) setTmpHashes(tmpHeights map[string]txInfo, key, value []byte, height int64) {
	index, eventSeq, err := extractIndexAndEventSeqFromKey(key)
	if err != nil {
		txi.log.Error("failure to parse index from key:", err)
		return
	}

	// Converting to string copies the value, which the iterator reuses.
	k := string(value) + string(eventSeq)
	info := txInfo{hash: k[:len(value)], height: height, index: index}
	// Keys of a tx indexed again at another position can share k, e.g. its
	// tx.height keys; keep the highest position, as uniqueTxInfos does.
	if prev, ok := tmpHeights[k]; ok && prev.after(info) {
		return
	}
	tmpHeights[k] = info
}

// match returns all matching txs by hash that meet a given condition and start
// key. An already filtered result (filteredHashes) is provided such that any
// non-intersecting matches are removed.
//
// NOTE: filteredHashes may be empty if no previous condition has matched.
func (txi *TxIndex) match(
	ctx context.Context,
	c syntax.Condition,
	startKeyBz []byte,
	filteredHashes map[string]txInfo,
	firstRun bool,
	heightInfo HeightInfo,
) map[string]txInfo {
	// A previous match was attempted but resulted in no matches, so we return
	// no matches (assuming AND operand).
	if !firstRun && len(filteredHashes) == 0 {
		return filteredHashes
	}

	tmpHashes := make(map[string]txInfo)

	switch c.Op {
	case syntax.TEq:
		it, err := dbm.IteratePrefix(txi.store, startKeyBz)
		if err != nil {
			panic(err)
		}
		defer it.Close()

	EQ_LOOP:
		for ; it.Valid(); it.Next() {

			// If we have a height range in a query, we need only transactions
			// for this height
			key := it.Key()
			keyHeight, err := extractHeightFromKey(key)
			if err != nil {
				txi.log.Error("failure to parse height from key:", err)
				continue
			}
			withinBounds, err := checkHeightConditions(heightInfo, keyHeight)
			if err != nil {
				txi.log.Error("failure checking for height bounds:", err)
				continue
			}
			if !withinBounds {
				continue
			}
			txi.setTmpHashes(tmpHashes, key, it.Value(), keyHeight)
			// Potentially exit early.
			select {
			case <-ctx.Done():
				break EQ_LOOP
			default:
			}
		}
		if err := it.Error(); err != nil {
			panic(err)
		}

	case syntax.TExists:
		// XXX: can't use startKeyBz here because c.Operand is nil
		// (e.g. "account.owner/<nil>/" won't match w/ a single row)
		it, err := dbm.IteratePrefix(txi.store, startKey(c.Tag))
		if err != nil {
			panic(err)
		}
		defer it.Close()

	EXISTS_LOOP:
		for ; it.Valid(); it.Next() {
			key := it.Key()
			keyHeight, err := extractHeightFromKey(key)
			if err != nil {
				txi.log.Error("failure to parse height from key:", err)
				continue
			}
			withinBounds, err := checkHeightConditions(heightInfo, keyHeight)
			if err != nil {
				txi.log.Error("failure checking for height bounds:", err)
				continue
			}
			if !withinBounds {
				continue
			}
			txi.setTmpHashes(tmpHashes, key, it.Value(), keyHeight)

			// Potentially exit early.
			select {
			case <-ctx.Done():
				break EXISTS_LOOP
			default:
			}
		}
		if err := it.Error(); err != nil {
			panic(err)
		}

	case syntax.TContains:
		// XXX: startKey does not apply here.
		// For example, if startKey = "account.owner/an/" and search query = "account.owner CONTAINS an"
		// we can't iterate with prefix "account.owner/an/" because we might miss keys like "account.owner/Ulan/"
		it, err := dbm.IteratePrefix(txi.store, startKey(c.Tag))
		if err != nil {
			panic(err)
		}
		defer it.Close()

	CONTAINS_LOOP:
		for ; it.Valid(); it.Next() {
			if !isTagKey(it.Key()) {
				continue
			}

			if strings.Contains(extractValueFromKey(it.Key()), c.Arg.Value()) {
				key := it.Key()
				keyHeight, err := extractHeightFromKey(key)
				if err != nil {
					txi.log.Error("failure to parse height from key:", err)
					continue
				}
				withinBounds, err := checkHeightConditions(heightInfo, keyHeight)
				if err != nil {
					txi.log.Error("failure checking for height bounds:", err)
					continue
				}
				if !withinBounds {
					continue
				}
				txi.setTmpHashes(tmpHashes, key, it.Value(), keyHeight)
			}

			// Potentially exit early.
			select {
			case <-ctx.Done():
				break CONTAINS_LOOP
			default:
			}
		}
		if err := it.Error(); err != nil {
			panic(err)
		}
	default:
		panic("other operators should be handled already")
	}

	if len(tmpHashes) == 0 || firstRun {
		// Either:
		//
		// 1. Regardless if a previous match was attempted, which may have had
		// results, but no match was found for the current condition, then we
		// return no matches (assuming AND operand).
		//
		// 2. A previous match was not attempted, so we return all results.
		return tmpHashes
	}

	// Remove/reduce matches in filteredHashes that were not found in this
	// match (tmpHashes).
REMOVE_LOOP:
	for k, v := range filteredHashes {
		tmpHash, ok := tmpHashes[k]
		if !ok || tmpHash.hash != v.hash {
			delete(filteredHashes, k)

			// Potentially exit early.
			select {
			case <-ctx.Done():
				break REMOVE_LOOP
			default:
			}
		}
	}

	return filteredHashes
}

// matchRange returns all matching txs by hash that meet a given queryRange and
// start key. An already filtered result (filteredHashes) is provided such that
// any non-intersecting matches are removed.
//
// NOTE: filteredHashes may be empty if no previous condition has matched.
func (txi *TxIndex) matchRange(
	ctx context.Context,
	qr indexer.QueryRange,
	startKey []byte,
	filteredHashes map[string]txInfo,
	firstRun bool,
	heightInfo HeightInfo,
) map[string]txInfo {
	// A previous match was attempted but resulted in no matches, so we return
	// no matches (assuming AND operand).
	if !firstRun && len(filteredHashes) == 0 {
		return filteredHashes
	}

	tmpHashes := make(map[string]txInfo)

	it, err := dbm.IteratePrefix(txi.store, startKey)
	if err != nil {
		panic(err)
	}
	defer it.Close()
	bigIntValue := new(big.Int)

LOOP:
	for ; it.Valid(); it.Next() {
		// TODO: We need to make a function for getting it.Key() as a byte slice with no copies.
		// It currently copies the source data (which can change on a subsequent .Next() call) but that
		// is not an issue for us.
		key := it.Key()
		if !isTagKey(key) {
			continue
		}

		if _, ok := qr.AnyBound().(*big.Float); ok {
			value := extractValueFromKey(key)
			v, ok := bigIntValue.SetString(value, 10)
			var vF *big.Float
			if !ok {
				vF, _, err = big.ParseFloat(value, 10, 125, big.ToNearestEven)
				if err != nil {
					continue LOOP
				}

			}
			keyHeight, err := extractHeightFromKey(key)
			if err != nil {
				txi.log.Error("failure to parse height from key:", err)
				continue
			}
			if qr.Key != types.TxHeightKey {
				withinBounds, err := checkHeightConditions(heightInfo, keyHeight)
				if err != nil {
					txi.log.Error("failure checking for height bounds:", err)
					continue
				}
				if !withinBounds {
					continue
				}
			}
			var withinBounds bool
			if !ok {
				withinBounds, err = idxutil.CheckBounds(qr, vF)
			} else {
				withinBounds, err = idxutil.CheckBounds(qr, v)
			}
			if err != nil {
				txi.log.Error("failed to parse bounds:", err)
			} else if withinBounds {
				txi.setTmpHashes(tmpHashes, key, it.Value(), keyHeight)
			}

			// XXX: passing time in a ABCI Events is not yet implemented
			// case time.Time:
			// 	v := strconv.ParseInt(extractValueFromKey(it.Key()), 10, 64)
			// 	if v == r.upperBound {
			// 		break
			// 	}
		}

		// Potentially exit early.
		select {
		case <-ctx.Done():
			break LOOP
		default:
		}
	}
	if err := it.Error(); err != nil {
		panic(err)
	}

	if len(tmpHashes) == 0 || firstRun {
		// Either:
		//
		// 1. Regardless if a previous match was attempted, which may have had
		// results, but no match was found for the current condition, then we
		// return no matches (assuming AND operand).
		//
		// 2. A previous match was not attempted, so we return all results.
		return tmpHashes
	}

	// Remove/reduce matches in filteredHashes that were not found in this
	// match (tmpHashes).
REMOVE_LOOP:
	for k, v := range filteredHashes {
		tmpHash, ok := tmpHashes[k]
		if !ok || tmpHash.hash != v.hash {
			delete(filteredHashes, k)

			// Potentially exit early.
			select {
			case <-ctx.Done():
				break REMOVE_LOOP
			default:
			}
		}
	}

	return filteredHashes
}

// Keys

func isTagKey(key []byte) bool {
	// Normally, if the event was indexed with an event sequence, the number of
	// tags should 4. Alternatively it should be 3 if the event was not indexed
	// with the corresponding event sequence. However, some attribute values in
	// production can contain the tag separator. Therefore, the condition is >= 3.
	numTags := 0
	for i := 0; i < len(key); i++ {
		if key[i] == tagKeySeparatorRune {
			numTags++
			if numTags >= 3 {
				return true
			}
		}
	}
	return false
}

func extractHeightFromKey(key []byte) (int64, error) {
	// the height is the second last element in the key.
	// Find the position of the last occurrence of tagKeySeparator
	endPos := bytes.LastIndexByte(key, tagKeySeparatorRune)
	if endPos == -1 {
		return 0, errors.New("separator not found")
	}

	// Find the position of the second last occurrence of tagKeySeparator
	startPos := bytes.LastIndexByte(key[:endPos-1], tagKeySeparatorRune)
	if startPos == -1 {
		return 0, errors.New("second last separator not found")
	}

	// Extract the height part of the key
	height, err := strconv.ParseInt(string(key[startPos+1:endPos]), 10, 64)
	if err != nil {
		return 0, err
	}
	return height, nil
}

// extractIndexAndEventSeqFromKey parses the last element of keys of the form
// <key>/<value>/<height>/<index>[$es$<eventSeq>]. eventSeq is a subslice of
// key; keys indexed without an event sequence yield "0".
func extractIndexAndEventSeqFromKey(key []byte) (uint32, []byte, error) {
	lastElem := key[bytes.LastIndexByte(key, tagKeySeparatorRune)+1:]
	indexBz, eventSeq, found := bytes.Cut(lastElem, []byte(eventSeqSeparator))
	if !found {
		eventSeq = []byte("0")
	}
	index, err := strconv.ParseUint(string(indexBz), 10, 32)
	if err != nil {
		return 0, nil, err
	}
	return uint32(index), eventSeq, nil
}

func extractValueFromKey(key []byte) string {
	// Find the positions of tagKeySeparator in the byte slice
	var indices []int
	for i, b := range key {
		if b == tagKeySeparatorRune {
			indices = append(indices, i)
		}
	}

	// If there are less than 2 occurrences of tagKeySeparator, return an empty string
	if len(indices) < 2 {
		return ""
	}

	// Extract the value between the first and second last occurrence of tagKeySeparator
	value := key[indices[0]+1 : indices[len(indices)-2]]

	// Trim any leading or trailing whitespace
	value = bytes.TrimSpace(value)

	// TODO: Do an unsafe cast to avoid an extra allocation here
	return string(value)
}

func keyForEvent(key string, value string, result *abci.TxResult, eventSeq int64) []byte {
	return []byte(fmt.Sprintf("%s/%s/%d/%d%s",
		key,
		value,
		result.Height,
		result.Index,
		eventSeqSeparator+strconv.FormatInt(eventSeq, 10),
	))
}

func keyForHeight(result *abci.TxResult) []byte {
	return []byte(fmt.Sprintf("%s/%d/%d/%d%s",
		types.TxHeightKey,
		result.Height,
		result.Height,
		result.Index,
		// Added to facilitate having the eventSeq in event keys
		// Otherwise queries break expecting 5 entries
		eventSeqSeparator+"0",
	))
}

func startKeyForCondition(c syntax.Condition, height int64) []byte {
	if height > 0 {
		return startKey(c.Tag, c.Arg.Value(), height)
	}
	return startKey(c.Tag, c.Arg.Value())
}

func startKey(fields ...interface{}) []byte {
	var b bytes.Buffer
	for _, f := range fields {
		b.Write([]byte(fmt.Sprintf("%v", f) + tagKeySeparator))
	}
	return b.Bytes()
}
