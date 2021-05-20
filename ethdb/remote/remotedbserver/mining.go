package remotedbserver

import (
	"bytes"
	"context"
	"errors"
	"sync"

	"github.com/ledgerwatch/erigon/common"
	"github.com/ledgerwatch/erigon/common/hexutil"
	"github.com/ledgerwatch/erigon/consensus/ethash"
	"github.com/ledgerwatch/erigon/core"
	"github.com/ledgerwatch/erigon/core/types"
	proto_txpool "github.com/ledgerwatch/erigon/gointerfaces/txpool"
	types2 "github.com/ledgerwatch/erigon/gointerfaces/types"
	"github.com/ledgerwatch/erigon/log"
	"github.com/ledgerwatch/erigon/rlp"
	"google.golang.org/protobuf/types/known/emptypb"
)

type MiningServer struct {
	proto_txpool.UnimplementedMiningServer
	ctx                 context.Context
	pendingLogsStreams  PendingLogsStreams
	pendingBlockStreams PendingBlockStreams
	minedBlockStreams   MinedBlockStreams
	ethash              *ethash.API
	eth                 core.EthBackend
}

func NewMiningServer(ctx context.Context, eth core.EthBackend, ethashApi *ethash.API) *MiningServer {
	return &MiningServer{ctx: ctx, eth: eth, ethash: ethashApi}
}

func (s *MiningServer) Version(context.Context, *emptypb.Empty) (*types2.VersionReply, error) {
	return &types2.VersionReply{Major: 1, Minor: 0, Patch: 0}, nil
}

func (s *MiningServer) GetWork(context.Context, *proto_txpool.GetWorkRequest) (*proto_txpool.GetWorkReply, error) {
	if s.ethash == nil {
		return nil, errors.New("not supported, consensus engine is not ethash")
	}
	res, err := s.ethash.GetWork()
	if err != nil {
		return nil, err
	}
	return &proto_txpool.GetWorkReply{HeaderHash: res[0], SeedHash: res[1], Target: res[2], BlockNumber: res[3]}, nil
}

func (s *MiningServer) SubmitWork(_ context.Context, req *proto_txpool.SubmitWorkRequest) (*proto_txpool.SubmitWorkReply, error) {
	if s.ethash == nil {
		return nil, errors.New("not supported, consensus engine is not ethash")
	}
	var nonce types.BlockNonce
	copy(nonce[:], req.BlockNonce)
	ok := s.ethash.SubmitWork(nonce, common.BytesToHash(req.PowHash), common.BytesToHash(req.Digest))
	return &proto_txpool.SubmitWorkReply{Ok: ok}, nil
}

func (s *MiningServer) SubmitHashRate(_ context.Context, req *proto_txpool.SubmitHashRateRequest) (*proto_txpool.SubmitHashRateReply, error) {
	if s.ethash == nil {
		return nil, errors.New("not supported, consensus engine is not ethash")
	}
	ok := s.ethash.SubmitHashRate(hexutil.Uint64(req.Rate), common.BytesToHash(req.Id))
	return &proto_txpool.SubmitHashRateReply{Ok: ok}, nil
}

func (s *MiningServer) GetHashRate(_ context.Context, req *proto_txpool.HashRateRequest) (*proto_txpool.HashRateReply, error) {
	if s.ethash == nil {
		return nil, errors.New("not supported, consensus engine is not ethash")
	}
	return &proto_txpool.HashRateReply{HashRate: s.ethash.GetHashrate()}, nil
}

func (s *MiningServer) Mining(_ context.Context, req *proto_txpool.MiningRequest) (*proto_txpool.MiningReply, error) {
	if s.ethash == nil {
		return nil, errors.New("not supported, consensus engine is not ethash")
	}
	return &proto_txpool.MiningReply{Enabled: s.eth.IsMining(), Running: true}, nil
}

func (s *MiningServer) OnPendingLogs(req *proto_txpool.OnPendingLogsRequest, reply proto_txpool.Mining_OnPendingLogsServer) error {
	remove := s.pendingLogsStreams.Add(reply)
	defer remove()
	<-reply.Context().Done()
	return reply.Context().Err()
}

func (s *MiningServer) BroadcastPendingLogs(l types.Logs) error {
	b, err := rlp.EncodeToBytes(l)
	if err != nil {
		return err
	}
	reply := &proto_txpool.OnPendingBlockReply{RplBlock: b}
	s.pendingBlockStreams.Broadcast(reply)
	return nil
}

func (s *MiningServer) OnPendingBlock(req *proto_txpool.OnPendingBlockRequest, reply proto_txpool.Mining_OnPendingBlockServer) error {
	remove := s.pendingBlockStreams.Add(reply)
	defer remove()
	<-reply.Context().Done()
	return reply.Context().Err()
}

func (s *MiningServer) BroadcastPendingBlock(block *types.Block) error {
	var buf bytes.Buffer
	if err := block.EncodeRLP(&buf); err != nil {
		return err
	}
	reply := &proto_txpool.OnPendingBlockReply{RplBlock: buf.Bytes()}
	s.pendingBlockStreams.Broadcast(reply)
	return nil
}

func (s *MiningServer) OnMinedBlock(req *proto_txpool.OnMinedBlockRequest, reply proto_txpool.Mining_OnMinedBlockServer) error {
	remove := s.minedBlockStreams.Add(reply)
	defer remove()
	<-reply.Context().Done()
	return reply.Context().Err()
}

func (s *MiningServer) BroadcastMinedBlock(block *types.Block) error {
	var buf bytes.Buffer
	if err := block.EncodeRLP(&buf); err != nil {
		return err
	}
	reply := &proto_txpool.OnMinedBlockReply{RplBlock: buf.Bytes()}
	s.minedBlockStreams.Broadcast(reply)
	return nil
}

// MinedBlockStreams - it's safe to use this class as non-pointer
type MinedBlockStreams struct {
	sync.Mutex
	id    uint
	chans map[uint]proto_txpool.Mining_OnMinedBlockServer
}

func (s *MinedBlockStreams) Add(stream proto_txpool.Mining_OnMinedBlockServer) (remove func()) {
	s.Lock()
	defer s.Unlock()
	if s.chans == nil {
		s.chans = make(map[uint]proto_txpool.Mining_OnMinedBlockServer)
	}
	s.id++
	id := s.id
	s.chans[id] = stream
	return func() { s.remove(id) }
}

func (s *MinedBlockStreams) Broadcast(reply *proto_txpool.OnMinedBlockReply) {
	s.Lock()
	defer s.Unlock()
	for id, stream := range s.chans {
		err := stream.Send(reply)
		if err != nil {
			log.Debug("failed send to mined block stream", "err", err)
			select {
			case <-stream.Context().Done():
				delete(s.chans, id)
			default:
			}
		}
	}
}

func (s *MinedBlockStreams) remove(id uint) {
	s.Lock()
	defer s.Unlock()
	_, ok := s.chans[id]
	if !ok { // double-unsubscribe support
		return
	}
	delete(s.chans, id)
}

// PendingBlockStreams - it's safe to use this class as non-pointer
type PendingBlockStreams struct {
	sync.Mutex
	id    uint
	chans map[uint]proto_txpool.Mining_OnPendingBlockServer
}

func (s *PendingBlockStreams) Add(stream proto_txpool.Mining_OnPendingBlockServer) (remove func()) {
	s.Lock()
	defer s.Unlock()
	if s.chans == nil {
		s.chans = make(map[uint]proto_txpool.Mining_OnPendingBlockServer)
	}
	s.id++
	id := s.id
	s.chans[id] = stream
	return func() { s.remove(id) }
}

func (s *PendingBlockStreams) Broadcast(reply *proto_txpool.OnPendingBlockReply) {
	s.Lock()
	defer s.Unlock()
	for id, stream := range s.chans {
		err := stream.Send(reply)
		if err != nil {
			log.Debug("failed send to mined block stream", "err", err)
			select {
			case <-stream.Context().Done():
				delete(s.chans, id)
			default:
			}
		}
	}
}

func (s *PendingBlockStreams) remove(id uint) {
	s.Lock()
	defer s.Unlock()
	_, ok := s.chans[id]
	if !ok { // double-unsubscribe support
		return
	}
	delete(s.chans, id)
}

// PendingLogsStreams - it's safe to use this class as non-pointer
type PendingLogsStreams struct {
	sync.Mutex
	id    uint
	chans map[uint]proto_txpool.Mining_OnPendingLogsServer
}

func (s *PendingLogsStreams) Add(stream proto_txpool.Mining_OnPendingLogsServer) (remove func()) {
	s.Lock()
	defer s.Unlock()
	if s.chans == nil {
		s.chans = make(map[uint]proto_txpool.Mining_OnPendingLogsServer)
	}
	s.id++
	id := s.id
	s.chans[id] = stream
	return func() { s.remove(id) }
}

func (s *PendingLogsStreams) Broadcast(reply *proto_txpool.OnPendingLogsReply) {
	s.Lock()
	defer s.Unlock()
	for id, stream := range s.chans {
		err := stream.Send(reply)
		if err != nil {
			log.Debug("failed send to mined block stream", "err", err)
			select {
			case <-stream.Context().Done():
				delete(s.chans, id)
			default:
			}
		}
	}
}

func (s *PendingLogsStreams) remove(id uint) {
	s.Lock()
	defer s.Unlock()
	_, ok := s.chans[id]
	if !ok { // double-unsubscribe support
		return
	}
	delete(s.chans, id)
}
