package erpc

import (
	"context"

	"github.com/erpc/erpc/indexer"
)

// cutFeed lets a tap end a batch at a block that left the pins after the
// batch's Read (a reorg while the tap sent it): the next Read starts from
// that block's parent instead of the batch's Next.
type cutFeed struct {
	indexer.ChainFeed
	from  indexer.Cursor
	isCut bool
}

func (f *cutFeed) Read(ctx context.Context, c indexer.Cursor, o indexer.ReadOpts) (indexer.Batch, error) {
	if f.isCut {
		c, f.isCut = f.from, false
	}
	return f.ChainFeed.Read(ctx, c, o)
}

// cut ends the batch before blk, an Applied block the tap did not send. Its
// parent is the last block the tap sent, or, for the batch's first block,
// the fork point its Reverted blocks went down to.
func (f *cutFeed) cut(blk indexer.Block) {
	f.from, f.isCut = indexer.Cursor{Number: blk.Number - min(blk.Number, 1), Hash: blk.ParentHash}, true
}
