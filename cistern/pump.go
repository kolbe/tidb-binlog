package cistern

import (
	"time"

	"github.com/juju/errors"
	"github.com/pingcap/tidb/store/tikv"
	"github.com/pingcap/tipb/go-binlog"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

// Result keeps the result of pulling binlog from a pump in a round
type Result struct {
	err       error
	nodeID    string
	clusterID uint64
	begin     binlog.Pos
	end       binlog.Pos
	binlogs   map[int64]*binlog.Binlog
}

// Pump holds the connection to a pump node, and keeps the savepoint of binlog last read
type Pump struct {
	nodeID    string
	clusterID uint64
	host      string
	conn      *grpc.ClientConn
	timeout   time.Duration
	client    binlog.PumpClient
	current   binlog.Pos
	batch     int32
	interval  time.Duration
}

// NewPump return an instance of Pump with opened gRPC connection
func NewPump(nodeID string, clusterID uint64, host string, timeout time.Duration, pos binlog.Pos, batch int32, interval time.Duration) (*Pump, error) {
	conn, err := grpc.Dial(host, grpc.WithInsecure(), grpc.WithTimeout(timeout))
	if err != nil {
		return nil, errors.Annotatef(err, "failed to connect to pump node(%s) at host(%s)", nodeID, host)
	}
	return &Pump{
		nodeID:    nodeID,
		clusterID: clusterID,
		host:      host,
		conn:      conn,
		timeout:   timeout,
		client:    binlog.NewPumpClient(conn),
		current:   pos,
		batch:     batch,
		interval:  interval,
	}, nil
}

// Close cuts off connection to pump server
func (p *Pump) Close() {
	p.conn.Close()
}

// Collect pulls a batch of binlog items from pump server, and records the begin and end position to the result.
// Note that the end position should be the next of the last one in batch.
// Each Prewrite type item in batch must find a type of Commit or Rollback one with the same startTS,
// if some ones don't find guys, it should pull another batch from pump and find their partners.
// Eventually, if there are still some rest ones, calls abort() via tikv client for them.
func (p *Pump) Collect(pctx context.Context, t *tikv.LockResolver) (res Result) {
	res = Result{
		nodeID:    p.nodeID,
		clusterID: p.clusterID,
		begin:     p.current,
		end:       p.current,
		binlogs:   make(map[int64]*binlog.Binlog),
	}

	ctx, cancel := context.WithTimeout(pctx, p.timeout)
	defer cancel()
	req := &binlog.PullBinlogReq{
		ClusterID: p.clusterID,
		StartFrom: p.current,
		Batch:     p.batch,
	}
	resp, err := p.client.PullBinlogs(ctx, req)
	if err != nil {
		res.err = errors.Trace(err)
		return
	}
	if resp.Errmsg != "" {
		res.err = errors.New(resp.Errmsg)
		return
	}
	if len(resp.Entities) == 0 {
		return
	}

	res.end = CalculateNextPos(resp.Entities[len(resp.Entities)-1])
	prewriteItems := make(map[int64]*binlog.Binlog)
	preDDLItems := make(map[int64]*binlog.Binlog)
	commitItems := make(map[int64]*binlog.Binlog)
	rollbackItems := make(map[int64]*binlog.Binlog)
	postDDLItems := make(map[int64]*binlog.Binlog)

	for _, item := range resp.Entities {
		b := new(binlog.Binlog)
		err := b.Unmarshal(item.Payload)
		if err != nil {
			res.err = errors.Annotatef(err, "unmarshal payload error, host(%s), clusterID(%s), Pos(%v)",
				p.host, p.clusterID, item.Pos)
			return
		}
		switch b.Tp {
		case binlog.BinlogType_Prewrite:
			prewriteItems[b.StartTs] = b
		case binlog.BinlogType_Commit:
			commitItems[b.StartTs] = b
		case binlog.BinlogType_Rollback:
			rollbackItems[b.StartTs] = b
		case binlog.BinlogType_PreDDL:
			preDDLItems[b.StartTs] = b
		case binlog.BinlogType_PostDDL:
			postDDLItems[b.StartTs] = b

		default:
			res.err = errors.Errorf("unrecognized binlog type(%d), host(%s), clusterID(%d), Pos(%v) ",
				b.Tp, p.host, p.clusterID, item.Pos)
		}
	}

	// match dml binlog
	for startTs, item := range prewriteItems {
		if co, ok := commitItems[startTs]; ok {
			item.CommitTs = co.CommitTs
			item.Tp = co.Tp
			res.binlogs[item.CommitTs] = item
			delete(prewriteItems, startTs)
		} else if _, ok := rollbackItems[startTs]; ok {
			delete(prewriteItems, startTs)
		}
	}

	// match ddl binlog
	for startTs, item := range preDDLItems {
		if postDDL, ok := postDDLItems[startTs]; ok {
			item.CommitTs = postDDL.CommitTs
			item.Tp = postDDL.Tp
			res.binlogs[item.CommitTs] = item
			delete(preDDLItems, startTs)
		}
	}

	// after an interval, pull a further batch from pump, and look up partners for the rest Prewrite items(if has)
	if len(prewriteItems) > 0 || len(preDDLItems) > 0 {
		if err := p.collectFurtherBatch(pctx, t, prewriteItems, preDDLItems, res.binlogs, res.end, 0); err != nil {
			res.err = errors.Trace(err)
			return
		}
	}

	return
}

func (p *Pump) collectFurtherBatch(pctx context.Context, t *tikv.LockResolver, prewriteItems, preDDLItems, binlogs map[int64]*binlog.Binlog, pos binlog.Pos, times int) error {
	if times > 3 {
		for startTs, item := range prewriteItems {
			primaryKey := item.GetPrewriteKey()
			status, err := t.GetTxnStatus(uint64(startTs), primaryKey)
			if err != nil {
				return errors.Trace(err)
			}
			if status.IsCommitted() {
				item.CommitTs = int64(status.CommitTS())
				item.Tp = binlog.BinlogType_Commit
				binlogs[item.CommitTs] = item
			}
			delete(prewriteItems, startTs)
		}
		if len(preDDLItems) > 0 {
			return errors.Errorf("can not find the matching postDDLItems with preDDLItems, %v", preDDLItems)
		}
		return nil
	}

	select {
	case <-pctx.Done():
		return errors.Trace(pctx.Err())
	case <-time.After(p.interval):
		ctx, cancel := context.WithTimeout(pctx, p.timeout)
		defer cancel()
		req := &binlog.PullBinlogReq{
			ClusterID: p.clusterID,
			StartFrom: pos,
			Batch:     p.batch,
		}
		resp, err := p.client.PullBinlogs(ctx, req)
		if err != nil {
			return errors.Trace(err)
		}
		if resp.Errmsg != "" {
			return errors.New(resp.Errmsg)
		}
		if len(resp.Entities) == 0 {
			return p.collectFurtherBatch(pctx, t, prewriteItems, preDDLItems, binlogs, pos, times+1)
		}

		pos = CalculateNextPos(resp.Entities[len(resp.Entities)-1])
		commitItems := make(map[int64]*binlog.Binlog)
		rollbackItems := make(map[int64]*binlog.Binlog)
		postDDLItems := make(map[int64]*binlog.Binlog)
		for _, item := range resp.Entities {
			b := new(binlog.Binlog)
			err := b.Unmarshal(item.Payload)
			if err != nil {
				return errors.Annotatef(err, "unmarshal payload error, host(%s), clusterID(%s), Pos(%v)",
					p.host, p.clusterID, item.Pos)
			}
			switch b.Tp {
			case binlog.BinlogType_Commit:
				commitItems[b.StartTs] = b
			case binlog.BinlogType_Rollback:
				rollbackItems[b.StartTs] = b
			case binlog.BinlogType_PostDDL:
				postDDLItems[b.StartTs] = b
			}
		}

		// match dml binlog
		for startTs, item := range prewriteItems {
			if co, ok := commitItems[startTs]; ok {
				item.CommitTs = co.CommitTs
				item.Tp = co.Tp
				binlogs[item.CommitTs] = item
				delete(prewriteItems, startTs)
			} else if _, ok := rollbackItems[startTs]; ok {
				delete(prewriteItems, startTs)
			}
		}

		// match ddl binlog
		for startTs, item := range preDDLItems {
			if postDDL, ok := postDDLItems[startTs]; ok {
				item.CommitTs = postDDL.CommitTs
				item.Tp = postDDL.Tp
				binlogs[item.CommitTs] = item
				delete(preDDLItems, startTs)
			}
		}

		if len(prewriteItems) > 0 || len(preDDLItems) > 0 {
			return p.collectFurtherBatch(pctx, t, prewriteItems, preDDLItems, binlogs, pos, times+1)
		}
	}

	return nil
}