/*
 * Copyright (C) 2017 Dgraph Labs, Inc. and Contributors
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package worker

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/dgraph-io/badger"
	"golang.org/x/net/context"
	"golang.org/x/net/trace"

	"github.com/dgraph-io/dgraph/algo"
	"github.com/dgraph-io/dgraph/group"
	"github.com/dgraph-io/dgraph/posting"
	"github.com/dgraph-io/dgraph/protos"
	"github.com/dgraph-io/dgraph/schema"
	"github.com/dgraph-io/dgraph/tok"
	"github.com/dgraph-io/dgraph/types"
	"github.com/dgraph-io/dgraph/x"
)

var emptySortResult protos.SortResult

type sortresult struct {
	reply *protos.SortResult
	vals  [][]types.Val
	err   error
}

// SortOverNetwork sends sort query over the network.
func SortOverNetwork(ctx context.Context, q *protos.SortMessage) (*protos.SortResult, error) {
	gid := group.BelongsTo(q.Attr[0])
	if tr, ok := trace.FromContext(ctx); ok {
		tr.LazyPrintf("worker.Sort attr: %v groupId: %v", q.Attr, gid)
	}

	if groups().ServesGroup(gid) {
		// No need for a network call, as this should be run from within this instance.
		return processSort(ctx, q)
	}

	result, err := processWithBackupRequest(ctx, gid, func(ctx context.Context, c protos.WorkerClient) (interface{}, error) {
		return c.Sort(ctx, q)
	})
	if err != nil {
		if tr, ok := trace.FromContext(ctx); ok {
			tr.LazyPrintf("Error while calling worker.Sort: %v", err)
		}
		return nil, err
	}
	return result.(*protos.SortResult), nil
}

// Sort is used to sort given UID matrix.
func (w *grpcWorker) Sort(ctx context.Context, s *protos.SortMessage) (*protos.SortResult, error) {
	if ctx.Err() != nil {
		return &emptySortResult, ctx.Err()
	}

	gid := group.BelongsTo(s.Attr[0])
	if tr, ok := trace.FromContext(ctx); ok {
		tr.LazyPrintf("Sorting: Attribute: %q groupId: %v Sort", s.Attr, gid)
	}

	var reply *protos.SortResult
	x.AssertTruef(groups().ServesGroup(gid),
		"attr: %q groupId: %v Request sent to wrong server.", s.Attr, gid)

	c := make(chan error, 1)
	go func() {
		var err error
		reply, err = processSort(ctx, s)
		c <- err
	}()

	select {
	case <-ctx.Done():
		return &emptySortResult, ctx.Err()
	case err := <-c:
		return reply, err
	}
}

var (
	errContinue = x.Errorf("Continue processing buckets")
	errDone     = x.Errorf("Done processing buckets")
)

func sortWithoutIndex(ctx context.Context, ts *protos.SortMessage) *sortresult {
	// TODO - Handle multisort with pagination here.
	n := len(ts.UidMatrix)
	r := new(protos.SortResult)
	// Sort and paginate directly as it'd be expensive to iterate over the index which
	// might have millions of keys just for retrieving some values.
	sType, err := schema.State().TypeOf(ts.Attr[0])
	if err != nil || !sType.IsScalar() {
		return &sortresult{&emptySortResult, nil,
			x.Errorf("Cannot sort attribute %s of type object.", ts.Attr)}
	}

	for i := 0; i < n; i++ {
		select {
		case <-ctx.Done():
			return &sortresult{&emptySortResult, nil, ctx.Err()}
		default:
			// Copy, otherwise it'd affect the destUids and hence the srcUids of Next level.
			tempList := &protos.List{ts.UidMatrix[i].Uids}
			// TODO - Get values also and send to paginate.
			if _, err := sortByValue(ctx, ts, tempList, sType); err != nil {
				return &sortresult{&emptySortResult, nil, err}
			}
			paginate(ts, tempList)
			r.UidMatrix = append(r.UidMatrix, tempList)
		}
	}
	return &sortresult{r, nil, nil}
}

func sortWithIndex(ctx context.Context, ts *protos.SortMessage) *sortresult {
	n := len(ts.UidMatrix)
	out := make([]intersectedList, n)
	values := make([][]types.Val, 0, n) // Values corresponding to uids in the uid matrix.
	for i := 0; i < n; i++ {
		// offsets[i] is the offset for i-th posting list. It gets decremented as we
		// iterate over buckets.
		out[i].offset = int(ts.Offset)
		// TODO - Define once.
		var emptyList protos.List
		out[i].ulist = &emptyList
	}
	r := new(protos.SortResult)
	// Iterate over every bucket / token.
	iterOpt := badger.DefaultIteratorOptions
	iterOpt.Reverse = ts.Desc[0]
	iterOpt.FetchValues = false
	it := pstore.NewIterator(iterOpt)
	defer it.Close()

	typ, err := schema.State().TypeOf(ts.Attr[0])
	if err != nil {
		return &sortresult{&emptySortResult, nil, fmt.Errorf("Attribute %s not defined in schema", ts.Attr)}
	}

	// Get the tokenizers and choose the corresponding one.
	if !schema.State().IsIndexed(ts.Attr[0]) {
		return &sortresult{&emptySortResult, nil, x.Errorf("Attribute %s is not indexed.", ts.Attr)}
	}

	tokenizers := schema.State().Tokenizer(ts.Attr[0])
	var tokenizer tok.Tokenizer
	for _, t := range tokenizers {
		// Get the first sortable index.
		if t.IsSortable() {
			tokenizer = t
			break
		}
	}

	if tokenizer == nil {
		// String type can have multiple tokenizers, only one of which is
		// sortable.
		if typ == types.StringID {
			return &sortresult{&emptySortResult, nil,
				x.Errorf("Attribute:%s does not have exact index for sorting.", ts.Attr[0])}
		}
		// Other types just have one tokenizer, so if we didn't find a
		// sortable tokenizer, then attribute isn't sortable.
		return &sortresult{&emptySortResult, nil, x.Errorf("Attribute:%s is not sortable.", ts.Attr)}
	}

	indexPrefix := x.IndexKey(ts.Attr[0], string(tokenizer.Identifier()))
	var seekKey []byte
	if !ts.Desc[0] {
		// We need to seek to the first key of this index type.
		seekKey = indexPrefix
	} else {
		// We need to reach the last key of this index type.
		seekKey = x.IndexKey(ts.Attr[0], string(tokenizer.Identifier()+1))
	}
	it.Seek(seekKey)

BUCKETS:

	// Outermost loop is over index buckets.
	for it.Valid() {
		key := it.Item().Key()
		if !bytes.HasPrefix(key, indexPrefix) {
			break
		}
		select {
		case <-ctx.Done():
			return &sortresult{&emptySortResult, nil, ctx.Err()}
		default:
			k := x.Parse(key)
			x.AssertTrue(k != nil)
			x.AssertTrue(k.IsIndex())
			token := k.Term
			if tr, ok := trace.FromContext(ctx); ok {
				tr.LazyPrintf("processSort: Token: %s", token)
			}
			// Intersect every UID list with the index bucket, and update their
			// results (in out).
			err := intersectBucket(ctx, ts, token, out)
			switch err {
			case errDone:
				break BUCKETS
			case errContinue:
				// Continue iterating over tokens / index buckets.
			default:
				return &sortresult{&emptySortResult, nil, err}
			}
			it.Next()
		}
	}

	for _, il := range out {
		r.UidMatrix = append(r.UidMatrix, il.ulist)
		if len(ts.Attr) > 1 {
			values = append(values, il.values)
		}
	}

	select {
	case <-ctx.Done():
		return &sortresult{&emptySortResult, nil, ctx.Err()}
	default:
		return &sortresult{r, values, nil}
	}
}

type orderResult struct {
	idx int
	r   *protos.Result
	err error
}

// processSort does sorting with pagination. It works by iterating over index
// buckets. As it iterates, it intersects with each UID list of the UID
// matrix. To optimize for pagination, we maintain the "offsets and sizes" or
// pagination window for each UID list. For each UID list, we ignore the
// bucket if we haven't hit the offset. We stop getting results when we got
// enough for our pagination params. When all the UID lists are done, we stop
// iterating over the index.
func processSort(ctx context.Context, ts *protos.SortMessage) (*protos.SortResult, error) {
	if ts.Count < 0 {
		return nil, x.Errorf("We do not yet support negative or infinite count with sorting: %s %d. "+
			"Try flipping order and return first few elements instead.", ts.Attr, ts.Count)
	}

	if schema.State().IsList(ts.Attr[0]) {
		return nil, x.Errorf("Sorting not supported on attr: %s of type: [scalar]", ts.Attr[0])
	}

	cctx, cancel := context.WithCancel(ctx)
	resCh := make(chan *sortresult, 2)
	go func() {
		select {
		case <-time.After(3 * time.Millisecond):
			// Wait between ctx chan and time chan.
		case <-ctx.Done():
			resCh <- &sortresult{err: ctx.Err()}
			return
		}
		r := sortWithoutIndex(cctx, ts)
		resCh <- r
	}()

	go func() {
		sr := sortWithIndex(cctx, ts)
		resCh <- sr
	}()

	r := <-resCh
	if r.err == nil {
		cancel()
		// wait for other goroutine to get cancelled
		<-resCh
	} else {
		if tr, ok := trace.FromContext(ctx); ok {
			tr.LazyPrintf(r.err.Error())
		}
		r = <-resCh
	}

	// If request didn't have multiple attributes or err was not nil we return.
	if !(len(ts.Attr) > 1) || r.err != nil {
		return r.reply, r.err
	}

	// SrcUids for other queries are all the uids present in the response of the first sort.
	destUids := destUids(r.reply.UidMatrix)

	// For each uid in dest uids, we have multiple values which belong to different attributes.
	// 1  -> [ "Alice", 23, "1932-01-01"]
	// 10 -> [ "Bob", 35, "1912-02-01" ]
	sortVals := make([][]types.Val, len(destUids.Uids))
	for idx := range sortVals {
		sortVals[idx] = make([]types.Val, len(ts.Attr))
	}

	seen := make(map[uint64]bool)
	// Walk through the uidMatrix and put values for this attribute in sortVals.
	for i, ul := range r.reply.UidMatrix {
		x.AssertTrue(len(ul.Uids) == len(r.vals[i]))
		for j, uid := range ul.Uids {
			uidx := algo.IndexOf(destUids, uid)
			x.AssertTrue(uidx >= 0)

			if seen[uid] {
				// We have already seen this uid.
				continue
			}
			seen[uid] = true

			sortVals[uidx][0] = r.vals[i][j]
		}
	}

	// Execute rest of the orders concurrently.
	och := make(chan orderResult, len(ts.Attr)-1)
	for i := 1; i < len(ts.Attr); i++ {
		attr := ts.Attr[i]
		in := &protos.Query{
			Attr:    attr,
			UidList: destUids,
		}
		go fetchValues(ctx, in, i, och)
	}

	var oerr error
	for i := 1; i < len(ts.Attr); i++ {
		or := <-och
		if or.err != nil && oerr == nil {
			oerr = or.err
			continue
		}

		result := or.r
		x.AssertTrue(len(result.ValueMatrix) == len(destUids.Uids))
		seen = map[uint64]bool{}
		for i, uid := range destUids.Uids {
			if seen[uid] {
				continue
			}
			v := result.ValueMatrix[i].Values[0]
			val := types.ValueForType(types.TypeID(v.ValType))
			var sv types.Val
			if bytes.Equal(v.Val, x.Nilbyte) {
				// Assign nil value which is sorted as greater than all other values.
				sv.Value = nil
				sv.Tid = val.Tid
			} else {
				val.Value = v.Val
				var err error
				sv, err = types.Convert(val, val.Tid)
				if err != nil {
					return r.reply, err
				}
			}
			seen[uid] = true
			sortVals[i][or.idx] = sv
		}
	}

	if oerr != nil {
		return r.reply, oerr
	}

	// Values have been accumulated, now we do the multisort for each list.
	for i, ul := range r.reply.UidMatrix {
		vals := make([][]types.Val, len(ul.Uids))
		for j, uid := range ul.Uids {
			idx := algo.IndexOf(destUids, uid)
			x.AssertTrue(idx >= 0)
			vals[j] = sortVals[idx]
		}
		types.Sort(vals, ul, ts.Desc)
		x.AssertTrue(len(ul.Uids) >= int(ts.Count))
		// Paginate
		ul.Uids = ul.Uids[:ts.Count]
		r.reply.UidMatrix[i] = ul
	}

	return r.reply, oerr
}

func destUids(uidMatrix []*protos.List) *protos.List {
	included := make(map[uint64]bool)
	for _, ul := range uidMatrix {
		for _, uid := range ul.Uids {
			if included[uid] {
				continue
			}
			included[uid] = true
		}
	}

	res := &protos.List{Uids: make([]uint64, 0, len(included))}
	for uid := range included {
		res.Uids = append(res.Uids, uid)
	}
	sort.Slice(res.Uids, func(i, j int) bool { return res.Uids[i] < res.Uids[j] })
	return res
}

func fetchValues(ctx context.Context, in *protos.Query, idx int, or chan orderResult) {
	var err error
	in.Reverse = strings.HasPrefix(in.Attr, "~")
	if in.Reverse {
		in.Attr = strings.TrimPrefix(in.Attr, "~")
	}
	r, err := ProcessTaskOverNetwork(ctx, in)
	// TODO - Use context here.
	or <- orderResult{
		idx: idx,
		err: err,
		r:   r,
	}
}

type intersectedList struct {
	offset int
	ulist  *protos.List
	values []types.Val
}

// intersectBucket intersects every UID list in the UID matrix with the
// indexed bucket.
func intersectBucket(ctx context.Context, ts *protos.SortMessage, token string,
	out []intersectedList) error {
	count := int(ts.Count)
	attr := ts.Attr[0]
	sType, err := schema.State().TypeOf(attr)
	if err != nil || !sType.IsScalar() {
		return x.Errorf("Cannot sort attribute %s of type object.", attr)
	}
	scalar := sType

	key := x.IndexKey(attr, token)
	// Don't put the Index keys in memory.
	pl := posting.Get(key)
	var vals []types.Val

	// For each UID list, we need to intersect with the index bucket.
	for i, ul := range ts.UidMatrix {
		il := &out[i]
		if count > 0 && len(il.ulist.Uids) >= count {
			continue
		}

		// Intersect index with i-th input UID list.
		listOpt := posting.ListOptions{
			Intersect: ul,
		}
		result := pl.Uids(listOpt) // The actual intersection work is done here.
		n := len(result.Uids)

		// Check offsets[i].
		if il.offset >= n {
			// We are going to skip the whole intersection. No need to do actual
			// sorting. Just update offsets[i]. We now offset less.
			il.offset -= n
			continue
		}

		// We are within the page. We need to apply sorting.
		// Sort results by value before applying offset.
		if vals, err = sortByValue(ctx, ts, result, scalar); err != nil {
			return err
		}

		// Result set might have reduced after sorting. As some uids might not have a
		// value in the lang specified.
		n = len(result.Uids)

		if il.offset > 0 {
			// Apply the offset.
			result.Uids = result.Uids[il.offset:n]
			if len(ts.Attr) > 1 {
				vals = vals[il.offset:n]
			}
			il.offset = 0
			n = len(result.Uids)
		}

		// n is number of elements to copy from result to out.
		// In case of multiple sort, we dont wan't to apply the count and copy all uids for the
		// current bucket.
		if count > 0 && (len(ts.Attr) == 1) {
			slack := count - len(il.ulist.Uids)
			if slack < n {
				n = slack
			}
		}

		il.ulist.Uids = append(il.ulist.Uids, result.Uids[:n]...)
		if len(ts.Attr) > 1 {
			il.values = append(il.values, vals[:n]...)
		}
	} // end for loop over UID lists in UID matrix.

	// Check out[i] sizes for all i.
	for i := 0; i < len(ts.UidMatrix); i++ { // Iterate over UID lists.
		if len(out[i].ulist.Uids) < count {
			return errContinue
		}

		if len(ts.Attr) == 1 {
			x.AssertTruef(len(out[i].ulist.Uids) == count, "%d %d", len(out[i].ulist.Uids), count)
		}
	}
	// All UID lists have enough items (according to pagination). Let's notify
	// the outermost loop.
	return errDone
}

func paginate(ts *protos.SortMessage, dest *protos.List) {
	count := int(ts.Count)
	offset := int(ts.Offset)
	start, end := x.PageRange(count, offset, len(dest.Uids))
	dest.Uids = dest.Uids[start:end]
}

// sortByValue fetches values and sort UIDList.
func sortByValue(ctx context.Context, ts *protos.SortMessage, ul *protos.List,
	typ types.TypeID) ([]types.Val, error) {
	lenList := len(ul.Uids)
	uids := make([]uint64, 0, lenList)
	values := make([][]types.Val, 0, lenList)
	multiSortVals := make([]types.Val, 0, lenList)
	for i := 0; i < lenList; i++ {
		select {
		case <-ctx.Done():
			return multiSortVals, ctx.Err()
		default:
			uid := ul.Uids[i]
			val, err := fetchValue(uid, ts.Attr[0], ts.Langs, typ)
			if err != nil {
				// If a value is missing, skip that UID in the result.
				continue
			}
			uids = append(uids, uid)
			values = append(values, []types.Val{val})
			if len(ts.Attr) > 1 {
				multiSortVals = append(multiSortVals, val)
			}
		}
	}
	err := types.Sort(values, &protos.List{uids}, []bool{ts.Desc[0]})
	ul.Uids = uids
	if len(ts.Attr) > 1 {
		x.AssertTrue(len(ul.Uids) == len(multiSortVals))
	}
	return multiSortVals, err
}

// fetchValue gets the value for a given UID.
func fetchValue(uid uint64, attr string, langs []string, scalar types.TypeID) (types.Val, error) {
	// Don't put the values in memory
	pl := posting.Get(x.DataKey(attr, uid))

	src, err := pl.ValueFor(langs)

	if err != nil {
		return types.Val{}, err
	}
	dst, err := types.Convert(src, scalar)
	if err != nil {
		return types.Val{}, err
	}

	return dst, nil
}
