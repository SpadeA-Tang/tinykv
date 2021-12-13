package raftstore

import (
	"fmt"
	"github.com/pingcap-incubator/tinykv/kv/raftstore/meta"
	"github.com/pingcap-incubator/tinykv/kv/util/engine_util"
	pb "github.com/pingcap-incubator/tinykv/proto/pkg/eraftpb"
	"reflect"
	"time"

	"github.com/Connor1996/badger/y"
	"github.com/pingcap-incubator/tinykv/kv/raftstore/message"
	"github.com/pingcap-incubator/tinykv/kv/raftstore/runner"
	"github.com/pingcap-incubator/tinykv/kv/raftstore/snap"
	"github.com/pingcap-incubator/tinykv/kv/raftstore/util"
	"github.com/pingcap-incubator/tinykv/log"
	"github.com/pingcap-incubator/tinykv/proto/pkg/metapb"
	"github.com/pingcap-incubator/tinykv/proto/pkg/raft_cmdpb"
	rspb "github.com/pingcap-incubator/tinykv/proto/pkg/raft_serverpb"
	"github.com/pingcap-incubator/tinykv/scheduler/pkg/btree"
	"github.com/pingcap/errors"
)

type PeerTick int

const raft_cmd = true

func dprint(flag bool, format string, a ...interface{}) {
	if flag {
		fmt.Printf(format, a...)
	}
}

const (
	PeerTickRaft               PeerTick = 0
	PeerTickRaftLogGC          PeerTick = 1
	PeerTickSplitRegionCheck   PeerTick = 2
	PeerTickSchedulerHeartbeat PeerTick = 3
)

type peerMsgHandler struct {
	*peer
	ctx *GlobalContext
}

func newPeerMsgHandler(peer *peer, ctx *GlobalContext) *peerMsgHandler {
	return &peerMsgHandler{
		peer: peer,
		ctx:  ctx,
	}
}

func (d *peerMsgHandler) mayUpdateRegion(res *ApplySnapResult) {
	if res != nil && !reflect.DeepEqual(res.PrevRegion, res.Region) {
		d.peerStorage.SetRegion(res.Region)
		storeMeta := d.ctx.storeMeta
		storeMeta.Lock()
		storeMeta.regionRanges.Delete(&regionItem{region: res.PrevRegion})
		storeMeta.regionRanges.ReplaceOrInsert(&regionItem{region: res.Region})
		storeMeta.regions[res.Region.GetId()] = res.Region
		storeMeta.Unlock()
	}
}

func (d *peerMsgHandler) HandleRaftReady() {
	if d.stopped {
		return
	}
	// Your Code Here (2B).
	if d.RaftGroup.HasReady() {
		rd := d.RaftGroup.Ready()
		res, err := d.peerStorage.SaveReadyState(&rd)
		if err != nil {
			panic("unimpl")
		}
		d.mayUpdateRegion(res)
		d.Send(d.ctx.trans, rd.Messages)
		d.HandleCommitEnts(rd.CommittedEntries)
		d.RaftGroup.Advance(rd)
	}
}

func (d *peerMsgHandler) handleRequest(
	msg raft_cmdpb.RaftCmdRequest, ent pb.Entry, wb *engine_util.WriteBatch) *engine_util.WriteBatch {

	if len(msg.Requests) != 1 {
		panic("msg.Request not equal to 1")
	}

	req := msg.Requests[0]
	key := getKey(req)
	if key != nil {
		err := util.CheckKeyInRegion(key, d.Region())
		if err != nil {
			panic(err)
		}
	}
	if msg.Header.RegionEpoch.Version != d.Region().GetRegionEpoch().Version {
		panic("Epoch conflict")
	}

	switch req.CmdType {
	case raft_cmdpb.CmdType_Put:
		dprint(raft_cmd, "[%d] has put key %v value %v\n", d.RaftGroup.Raft.GetId(), string(req.Put.Key), string(req.Put.Value))
		wb.SetCF(req.Put.Cf, req.Put.Key, req.Put.Value)
	case raft_cmdpb.CmdType_Delete:
		dprint(raft_cmd, "[%d] has delete key %v\n", d.RaftGroup.Raft.GetId(), string(req.Delete.Key))
		wb.DeleteCF(req.Delete.Cf, req.Delete.Key)
	case raft_cmdpb.CmdType_Snap:
		if msg.Header.RegionEpoch.Version != d.Region().RegionEpoch.Version {
			panic("todo")
			return wb
		}
	}
	err := wb.WriteToDB(d.ctx.engine.Kv)
	if err != nil {
		panic(err)
	}
	wb = &engine_util.WriteBatch{}

	return d.handleProposal(req, ent, wb)
}

func (d *peerMsgHandler) handleProposal(req *raft_cmdpb.Request, ent pb.Entry, wb *engine_util.WriteBatch) *engine_util.WriteBatch {

	p := d.popProposal(ent.Term, ent.Index)
	if p == nil {
		return wb
	}

	// p.term must be less than or equal to ent.Term due to the logic in popProposal
	if p.term != ent.Term {
		NotifyStaleReq(d.Term(), p.cb)
		return wb
	}

	if p.index != ent.Index {
		for p != nil {
			fmt.Printf("Normal request p.index %d p.term %d should be equal to ent.index %d ent.term %d\n", p.index, p.term, ent.Index, ent.Term)
			p = d.popProposal(ent.Term+100, ent.Index)
		}
		panic("something wrong")
	}

	//if len(d.proposals) == 0 {
	//	return wb
	//}
	//p := d.proposals[0]
	//
	//for len(d.proposals) > 1 && p.index < ent.Index {
	//	d.proposals = d.proposals[1:]
	//	NotifyStaleReq(ent.Term, p.cb)
	//	p = d.proposals[0]
	//}

	//fmt.Printf("[%d] handle proposal with index %d current has %d proposals\n",
	//	d.RaftGroup.Raft.GetId(), p.index, len(d.proposals))

	//if p.index == ent.Index && p.term != ent.Term {
	//	NotifyStaleReq(ent.Term, p.cb)
	//	return wb
	//}

	//if p.index != ent.Index || p.term != ent.Term { // not equal means p.index > ent.index
	//	fmt.Printf("Stale req p.index %d p.term %d ent.Index %d ent.term %d\n", p.index, p.term, ent.Index, ent.Term)
	//	//NotifyStaleReq(ent.Term, p.cb)
	//	return wb
	//}
	//d.proposals = d.proposals[1:]

	responses := []*raft_cmdpb.Response{}
	res := &raft_cmdpb.RaftCmdResponse{
		Header: &raft_cmdpb.RaftResponseHeader{},
	}
	switch req.CmdType {
	// for Get and Snap, we should first persist the wb which may
	// contain the previous effects
	case raft_cmdpb.CmdType_Get:
		//d.peerStorage.applyState.AppliedIndex = ent.Index
		//wb.SetMeta(meta.ApplyStateKey(d.regionId), d.peerStorage.applyState)
		//wb.WriteToDB(d.peerStorage.Engines.Kv)
		//wb = &engine_util.WriteBatch{}
		val, err := engine_util.GetCF(d.ctx.engine.Kv, req.Get.Cf, req.Get.Key)
		if err != nil {
			panic(err)
		}
		responses = append(responses, &raft_cmdpb.Response{
			CmdType: raft_cmdpb.CmdType_Get,
			Get: &raft_cmdpb.GetResponse{
				Value: val,
			},
		})
	case raft_cmdpb.CmdType_Put:
		responses = append(responses, &raft_cmdpb.Response{
			CmdType: raft_cmdpb.CmdType_Put,
			Put:     &raft_cmdpb.PutResponse{},
		})
	case raft_cmdpb.CmdType_Delete:
		responses = append(responses, &raft_cmdpb.Response{
			CmdType: raft_cmdpb.CmdType_Delete,
			Delete:  &raft_cmdpb.DeleteResponse{},
		})
	case raft_cmdpb.CmdType_Snap:
		//d.peerStorage.applyState.AppliedIndex = ent.Index
		//wb.SetMeta(meta.ApplyStateKey(d.regionId), d.peerStorage.applyState)
		//wb.WriteToDB(d.ctx.engine.Kv)
		//wb = &engine_util.WriteBatch{}
		responses = append(responses, &raft_cmdpb.Response{
			CmdType: raft_cmdpb.CmdType_Snap,
			Snap: &raft_cmdpb.SnapResponse{
				Region: d.Region(),
			},
		})

		// todo: what's the difference
		p.cb.Txn = d.ctx.engine.Kv.NewTransaction(false)
		//p.cb.Txn = d.peerStorage.Engines.Kv.NewTransaction(false)
	}
	res.Responses = responses

	p.cb.Done(res)
	return wb
}

func (d *peerMsgHandler) updateConfChange(cc *pb.ConfChange) {
	//
	region := new(metapb.Region)
	err := util.CloneMsg(d.Region(), region)
	if err != nil {
		log.Warnf("%s updateConfChange err:%s", d.Tag, err.Error())
		return
	}
	//预处理;
	switch cc.GetChangeType() {
	case pb.ConfChangeType_AddNode:
		d.addNode(region, cc)
	case pb.ConfChangeType_RemoveNode:
		d.removeNode(region, cc)
	default:
		log.Warnf("%s unknown ConfChangeType(%v)", d.Tag, cc.GetChangeType())
		return
	}
}

func (d *peerMsgHandler) addNode(region *metapb.Region, cc *pb.ConfChange) {
	nodeId := cc.GetNodeId()
	msg := &raft_cmdpb.RaftCmdRequest{}
	err := msg.Unmarshal(cc.Context)
	if err != nil {
		log.Errorf("%s addNode -%d unmarshal err:%s", d.Tag, nodeId, err.Error())
		return
	}
	//has add to peer.cache;
	peer := msg.AdminRequest.ChangePeer.Peer
	//region 已经存在，那么直接跳过.
	if util.FindPeer(region, peer.StoreId) != nil {
		//need do nothing;
		return
	}
	//set region
	region.Peers = append(region.Peers, peer)
	region.GetRegionEpoch().ConfVer++
	//save to db;
	var kvwb engine_util.WriteBatch
	meta.WriteRegionState(&kvwb, region, rspb.PeerState_Normal)
	err = kvwb.WriteToDB(d.peerStorage.Engines.Kv)
	if err != nil {
		return
	}
	//NOTICE-3B:修改具体的配置，放到落库成功——万一失败，那么就完全不需要回滚，直接就是原来到配置.
	d.insertPeerCache(peer)
	d.SetRegion(region)
}

func (d *peerMsgHandler) removeNode(region *metapb.Region, cc *pb.ConfChange) {
	if cc.NodeId == d.RaftGroup.Raft.GetId() {
		d.destroyPeer()
		return
	}
	wb := &engine_util.WriteBatch{}
	if region.RemovePeer(cc.NodeId) {
		region.RegionEpoch.ConfVer += 1
		//wb.SetMeta(meta.RegionStateKey(d.regionId), &rspb.RegionLocalState{
		//	State:  rspb.PeerState_Normal,
		//	Region: region,
		//})
		wb.SetMeta(meta.RegionStateKey(d.regionId), &rspb.RegionLocalState{
			State:  rspb.PeerState_Normal,
			Region: region,
		})
		d.ctx.storeMeta.Lock()
		d.ctx.storeMeta.regions[region.Id] = region
		d.ctx.storeMeta.Unlock()
		d.removePeerCache(cc.NodeId)
		wb.WriteToDB(d.peerStorage.Engines.Kv)
	}

	//nodeId := cc.GetNodeId()
	//msg := &raft_cmdpb.RaftCmdRequest{}
	//err := msg.Unmarshal(cc.Context)
	//if err != nil {
	//	return
	//}
	////find peer;
	//peer := d.peer.getPeerFromCache(nodeId)
	//if nil == util.RemovePeer(region, peer.GetStoreId()) {
	//	//need do nothing;
	//	return
	//}
	////set region
	//if d.Meta.GetId() != nodeId {
	//	region.GetRegionEpoch().ConfVer++
	//	util.RemovePeer(region, peer.GetStoreId())
	//} else {
	//	//clear peers;
	//	//region.Peers = []*metapb.Peer{}
	//	//NOTICE-3B:这里不需要置空，因为这里destroy之后，会重新new，用到是新到数据，这里有啥都无所谓来.
	//}
	////save to db;
	//if d.Meta.GetId() == nodeId {
	//	//d.SetRegion(region)
	//	d.destroyPeer() //save db inner;
	//	return
	//} else {
	//	//NOTICE-3B:destory 中会WriteRegionState.如果不调用destory，则需要手动调用save下.
	//	wb := &engine_util.WriteBatch{}
	//	wb.SetMeta(meta.RegionStateKey(d.regionId), &rspb.RegionLocalState{
	//		State:  rspb.PeerState_Normal,
	//		Region: region,
	//	})
	//	//d.ctx.storeMeta.Lock()
	//	d.ctx.storeMeta.regions[region.Id] = region
	//	//d.ctx.storeMeta.Unlock()
	//	d.removePeerCache(cc.NodeId)
	//	wb.WriteToDB(d.peerStorage.Engines.Kv)
	//
	//
	//	//var kvwb engine_util.WriteBatch
	//	//meta.WriteRegionState(&kvwb, region, rspb.PeerState_Normal)
	//	//err = kvwb.WriteToDB(d.peerStorage.Engines.Kv)
	//	//if err != nil {
	//	//	log.Errorf("%s AddNode(%s)writeToDB err:%s", d.Tag, nodeTag, err.Error())
	//	//} else {
	//	//	d.SetRegion(region)
	//	//}
	//}
}

func (d *peerMsgHandler) handleAdminRequest(
	msg raft_cmdpb.RaftCmdRequest, ent pb.Entry, wb *engine_util.WriteBatch) *engine_util.WriteBatch {
	switch msg.AdminRequest.CmdType {
	case raft_cmdpb.AdminCmdType_CompactLog:
		applySt := d.peerStorage.applyState
		compactLog := msg.AdminRequest.CompactLog
		if compactLog.CompactIndex > applySt.TruncatedState.Index {
			applySt.TruncatedState.Index = compactLog.CompactIndex
			applySt.TruncatedState.Term = compactLog.CompactTerm
			wb.SetMeta(meta.ApplyStateKey(d.regionId), applySt)
			d.ScheduleCompactLog(compactLog.CompactIndex)
			log.Infof("[%d] Compact log... truncate index: %v", d.RaftGroup.Raft.GetId(), applySt.TruncatedState.Index)
		}
	case raft_cmdpb.AdminCmdType_ChangePeer:
		cc := pb.ConfChange{}
		if err := cc.Unmarshal(ent.Data); err != nil {
			panic(err)
		}
		region := new(metapb.Region)
		err := util.CloneMsg(d.Region(), region)
		if err != nil {
			return wb
		}
		msg := &raft_cmdpb.RaftCmdRequest{}
		if err := msg.Unmarshal(cc.Context); err != nil {
			panic(err)
		}
		//var resp *raft_cmdpb.RaftCmdResponse

		if _, ok := util.CheckRegionEpoch(msg, region, true).(*util.ErrEpochNotMatch); ok {
			//resp = ErrResp(err)
			fmt.Printf("CheckRegionEpoch fail %v\n", msg.AdminRequest)
		} else {
			switch cc.ChangeType {
			case pb.ConfChangeType_AddNode:
				pr := msg.AdminRequest.ChangePeer.Peer
				//if util.FindPeer(region, pr.StoreId) != nil {
				//	//need do nothing;
				//	return wb
				//}
				if region.AddPeer(pr) {
					//log.Infof("[%d] adds node :%d", d.RaftGroup.Raft.GetId(), cc.NodeId)
					//region.Peers = append(region.Peers, pr)
					region.RegionEpoch.ConfVer += 1
					meta.WriteRegionState(wb, region, rspb.PeerState_Normal)
					//d.ctx.storeMeta.Lock()
					//d.ctx.storeMeta.regions[region.Id] = region
					//d.ctx.storeMeta.Unlock()
					err := wb.WriteToDB(d.peerStorage.Engines.Kv)
					if err != nil {
						panic("")
					}
					wb = &engine_util.WriteBatch{}
					d.insertPeerCache(pr)
					d.SetRegion(region)
				} else {
					return wb
				}
			case pb.ConfChangeType_RemoveNode:
				if cc.NodeId == d.RaftGroup.Raft.GetId() {
					//d.SetRegion(region)
					d.destroyPeer()
					return wb
				}
				if region.RemovePeer(cc.NodeId) {
					region.RegionEpoch.ConfVer += 1
					//wb.SetMeta(meta.RegionStateKey(d.regionId), &rspb.RegionLocalState{
					//	State:  rspb.PeerState_Normal,
					//	Region: region,
					//})
					wb.SetMeta(meta.RegionStateKey(d.regionId), &rspb.RegionLocalState{
						State:  rspb.PeerState_Normal,
						Region: region,
					})
					d.ctx.storeMeta.Lock()
					d.ctx.storeMeta.regions[region.Id] = region
					d.ctx.storeMeta.Unlock()
					d.removePeerCache(cc.NodeId)
					wb.WriteToDB(d.peerStorage.Engines.Kv)
				}
			}

			d.RaftGroup.ApplyConfChange(cc)
		}
	}

	return wb
}

func (d *peerMsgHandler) popProposal(term uint64, index uint64) *proposal {
	if len(d.proposals) == 0 {
		return nil
	}
	p := d.proposals[0]
	if p.term > term || (p.term == term && p.index > index) {
		return nil
	} else {
		d.proposals = d.proposals[1:]
		fmt.Printf("[%d] entry index %d current proposal: ", d.RaftGroup.Raft.GetId(), index)
		for _, prop := range d.proposals {
			fmt.Printf(" %d", prop.index)
		}
		fmt.Println()
		return p
	}
}

func (d *peerMsgHandler) HandleCommitEnts(ents []pb.Entry) {
	if len(ents) > 0 {
		wb := &engine_util.WriteBatch{}
		for _, ent := range ents {
			msg := raft_cmdpb.RaftCmdRequest{}
			if ent.Data != nil {
				if ent.EntryType != pb.EntryType_EntryConfChange {
					err := msg.Unmarshal(ent.Data)
					if err != nil {
						panic(err)
					}
				} else {
					// set it in order to enter d.handleAdminRequest()
					msg.AdminRequest = &raft_cmdpb.AdminRequest{
						CmdType: raft_cmdpb.AdminCmdType_ChangePeer,
					}

					var cc pb.ConfChange
					err := cc.Unmarshal(ent.GetData())
					if err != nil {
						log.Fatalf(`apply confChange err:%s`, err.Error())
					}
					d.RaftGroup.ApplyConfChange(cc)
					d.updateConfChange(&cc)
					continue
				}
				if msg.AdminRequest != nil {
					if len(msg.Requests) != 0 {
						panic("here")
					}
					wb = d.handleAdminRequest(msg, ent, wb)
				} else {
					wb = d.handleRequest(msg, ent, wb)
				}
			}
			// guess: when node is deleted, d.stopped will be true
			if d.stopped {
				return
			}
		}
		d.peerStorage.applyState.AppliedIndex = ents[len(ents)-1].Index
		wb.SetMeta(meta.ApplyStateKey(d.regionId), d.peerStorage.applyState)
		wb.WriteToDB(d.peerStorage.Engines.Kv)
	}
}

func (d *peerMsgHandler) HandleMsg(msg message.Msg) {
	switch msg.Type {
	case message.MsgTypeRaftMessage:
		raftMsg := msg.Data.(*rspb.RaftMessage)
		if err := d.onRaftMsg(raftMsg); err != nil {
			log.Errorf("%s handle raft message error %v", d.Tag, err)
		}
	// request 在此处进行封装，然后由leader propose后，最终在message.MsgTypeRaftMessage被添加到各个follower的Raftlog中
	case message.MsgTypeRaftCmd:
		raftCMD := msg.Data.(*message.MsgRaftCmd)
		d.proposeRaftCommand(raftCMD.Request, raftCMD.Callback)
	case message.MsgTypeTick:
		d.onTick()
	case message.MsgTypeSplitRegion:
		split := msg.Data.(*message.MsgSplitRegion)
		log.Infof("%s on split with %v", d.Tag, split.SplitKey)
		d.onPrepareSplitRegion(split.RegionEpoch, split.SplitKey, split.Callback)
	case message.MsgTypeRegionApproximateSize:
		d.onApproximateRegionSize(msg.Data.(uint64))
	case message.MsgTypeGcSnap:
		gcSnap := msg.Data.(*message.MsgGCSnap)
		d.onGCSnap(gcSnap.Snaps)
	case message.MsgTypeStart:
		d.startTicker()
	}
}

func (d *peerMsgHandler) preProposeRaftCommand(req *raft_cmdpb.RaftCmdRequest) error {
	// Check store_id, make sure that the msg is dispatched to the right place.
	if err := util.CheckStoreID(req, d.storeID()); err != nil {
		return err
	}

	// Check whether the store has the right peer to handle the request.
	regionID := d.regionId
	leaderID := d.LeaderId()
	if !d.IsLeader() {
		leader := d.getPeerFromCache(leaderID)
		return &util.ErrNotLeader{RegionId: regionID, Leader: leader}
	}
	// peer_id must be the same as peer's.
	if err := util.CheckPeerID(req, d.PeerId()); err != nil {
		return err
	}
	// Check whether the term is stale.
	if err := util.CheckTerm(req, d.Term()); err != nil {
		return err
	}
	err := util.CheckRegionEpoch(req, d.Region(), true)
	if errEpochNotMatching, ok := err.(*util.ErrEpochNotMatch); ok {
		// Attach the region which might be split from the current region. But it doesn't
		// matter if the region is not split from the current region. If the region meta
		// received by the TiKV driver is newer than the meta cached in the driver, the meta is
		// updated.
		siblingRegion := d.findSiblingRegion()
		if siblingRegion != nil {
			errEpochNotMatching.Regions = append(errEpochNotMatching.Regions, siblingRegion)
		}
		return errEpochNotMatching
	}
	return err
}

func (d *peerMsgHandler) proposeRequest(msg *raft_cmdpb.RaftCmdRequest, cb *message.Callback) {
	// sequentially record the callback(proposal) of each RaftCmdRequest
	//when the request is done, callback can wake the client and give the response
	d.proposals = append(d.proposals, &proposal{
		index: d.nextProposalIndex(),
		term:  d.Term(),
		cb:    cb,
	})
	//fmt.Printf("[%d] new proposal with index %d current has %d proposals\n",
	//	d.RaftGroup.Raft.GetId(), d.nextProposalIndex(), len(d.proposals))

	if len(msg.Requests) == 0 {
		fmt.Println()
	}

	// maybe unnecessary
	key := getKey(msg.Requests[0])
	if key != nil {
		err := util.CheckKeyInRegion(key, d.Region())
		if err != nil {
			panic(err)
		}
	}
	data, err := msg.Marshal()
	if err != nil {
		panic("")
	}

	err = d.RaftGroup.Propose(data)
	if err != nil {
		panic(err)
	}
}

func (d *peerMsgHandler) proposeAdminRequest(msg *raft_cmdpb.RaftCmdRequest, cb *message.Callback) {
	switch msg.AdminRequest.CmdType {
	case raft_cmdpb.AdminCmdType_CompactLog:
		data, err := msg.Marshal()
		if err != nil {
			panic(err)
		}
		d.RaftGroup.Propose(data)
	case raft_cmdpb.AdminCmdType_TransferLeader:
		peer := msg.AdminRequest.TransferLeader
		d.RaftGroup.TransferLeader(peer.Peer.Id)
		cb.AdminDone(raft_cmdpb.AdminCmdType_TransferLeader)
	case raft_cmdpb.AdminCmdType_ChangePeer:
		// One conf change is in progress
		if d.RaftGroup.Raft.PendingConfIndex > 0 {
			cb.AdminDone(raft_cmdpb.AdminCmdType_ChangePeer)
			return
		}

		cmd := msg.AdminRequest.ChangePeer
		data, err := msg.Marshal()
		if err != nil {
			panic(err)
		}

		cc := pb.ConfChange{
			NodeId:     cmd.Peer.Id,
			ChangeType: cmd.ChangeType,
			Context:    data,
		}

		err = d.RaftGroup.ProposeConfChange(cc)
		if err != nil {
			panic(err)
		}

		cb.AdminDone(raft_cmdpb.AdminCmdType_ChangePeer)
	}
}

func (d *peerMsgHandler) proposeRaftCommand(msg *raft_cmdpb.RaftCmdRequest, cb *message.Callback) {
	err := d.preProposeRaftCommand(msg)
	if err != nil {
		cb.Done(ErrResp(err))
		return
	}
	// Your Code Here (2B).
	if msg.AdminRequest != nil {
		fmt.Printf("[%d] propose cmd %v potential next index %d\n",
			d.RaftGroup.Raft.GetId(), msg.AdminRequest.CmdType, d.nextProposalIndex())
		d.proposeAdminRequest(msg, cb)
	} else {
		key := getKey(msg.Requests[0])
		fmt.Printf("[%d] propose cmd %v potential next index %d",
			d.RaftGroup.Raft.GetId(), msg.Requests[0].CmdType, d.nextProposalIndex())
		if key != nil {
			fmt.Printf(" with key %v", string(key))
			if msg.Requests[0].CmdType == raft_cmdpb.CmdType_Put {
				val := msg.Requests[0].Put.Value
				fmt.Printf(" with value %v", string(val))
			}
		}
		fmt.Println()

		d.proposeRequest(msg, cb)
	}

}

func getKey(req *raft_cmdpb.Request) []byte {
	switch req.CmdType {
	case raft_cmdpb.CmdType_Put:
		return req.Put.Key
	case raft_cmdpb.CmdType_Get:
		return req.Get.Key
	case raft_cmdpb.CmdType_Delete:
		return req.Delete.Key
	}
	return nil
}

func (d *peerMsgHandler) onTick() {
	if d.stopped {
		return
	}
	d.ticker.tickClock()
	if d.ticker.isOnTick(PeerTickRaft) {
		d.onRaftBaseTick()
	}
	if d.ticker.isOnTick(PeerTickRaftLogGC) {
		d.onRaftGCLogTick()
	}
	if d.ticker.isOnTick(PeerTickSchedulerHeartbeat) {
		d.onSchedulerHeartbeatTick()
	}
	if d.ticker.isOnTick(PeerTickSplitRegionCheck) {
		d.onSplitRegionCheckTick()
	}
	d.ctx.tickDriverSender <- d.regionId
}

func (d *peerMsgHandler) startTicker() {
	d.ticker = newTicker(d.regionId, d.ctx.cfg)
	d.ctx.tickDriverSender <- d.regionId
	d.ticker.schedule(PeerTickRaft)
	d.ticker.schedule(PeerTickRaftLogGC)
	d.ticker.schedule(PeerTickSplitRegionCheck)
	d.ticker.schedule(PeerTickSchedulerHeartbeat)
}

func (d *peerMsgHandler) onRaftBaseTick() {
	d.RaftGroup.Tick()
	d.ticker.schedule(PeerTickRaft)
}

func (d *peerMsgHandler) ScheduleCompactLog(truncatedIndex uint64) {
	raftLogGCTask := &runner.RaftLogGCTask{
		RaftEngine: d.ctx.engine.Raft,
		RegionID:   d.regionId,
		StartIdx:   d.LastCompactedIdx,
		EndIdx:     truncatedIndex + 1,
	}
	d.LastCompactedIdx = raftLogGCTask.EndIdx
	d.ctx.raftLogGCTaskSender <- raftLogGCTask
}

func (d *peerMsgHandler) onRaftMsg(msg *rspb.RaftMessage) error {
	log.Debugf("%s handle raft message %s from %d to %d",
		d.Tag, msg.GetMessage().GetMsgType(), msg.GetFromPeer().GetId(), msg.GetToPeer().GetId())
	if !d.validateRaftMessage(msg) {
		return nil
	}
	if d.stopped {
		return nil
	}
	if msg.GetIsTombstone() {
		// we receive a message tells us to remove self.
		d.handleGCPeerMsg(msg)
		return nil
	}
	if d.checkMessage(msg) {
		return nil
	}
	key, err := d.checkSnapshot(msg)
	if err != nil {
		return err
	}
	if key != nil {
		// If the snapshot file is not used again, then it's OK to
		// delete them here. If the snapshot file will be reused when
		// receiving, then it will fail to pass the check again, so
		// missing snapshot files should not be noticed.
		s, err1 := d.ctx.snapMgr.GetSnapshotForApplying(*key)
		if err1 != nil {
			return err1
		}
		d.ctx.snapMgr.DeleteSnapshot(*key, s, false)
		return nil
	}
	d.insertPeerCache(msg.GetFromPeer())
	err = d.RaftGroup.Step(*msg.GetMessage())
	if err != nil {
		return err
	}
	if d.AnyNewPeerCatchUp(msg.FromPeer.Id) {
		d.HeartbeatScheduler(d.ctx.schedulerTaskSender)
	}
	return nil
}

// return false means the message is invalid, and can be ignored.
func (d *peerMsgHandler) validateRaftMessage(msg *rspb.RaftMessage) bool {
	regionID := msg.GetRegionId()
	from := msg.GetFromPeer()
	to := msg.GetToPeer()
	log.Debugf("[region %d] handle raft message %s from %d to %d", regionID, msg, from.GetId(), to.GetId())
	if to.GetStoreId() != d.storeID() {
		log.Warnf("[region %d] store not match, to store id %d, mine %d, ignore it",
			regionID, to.GetStoreId(), d.storeID())
		return false
	}
	if msg.RegionEpoch == nil {
		log.Errorf("[region %d] missing epoch in raft message, ignore it", regionID)
		return false
	}
	return true
}

/// Checks if the message is sent to the correct peer.
///
/// Returns true means that the message can be dropped silently.
func (d *peerMsgHandler) checkMessage(msg *rspb.RaftMessage) bool {
	fromEpoch := msg.GetRegionEpoch()
	isVoteMsg := util.IsVoteMessage(msg.Message)
	fromStoreID := msg.FromPeer.GetStoreId()

	// Let's consider following cases with three nodes [1, 2, 3] and 1 is leader:
	// a. 1 removes 2, 2 may still send MsgAppendResponse to 1.
	//  We should ignore this stale message and let 2 remove itself after
	//  applying the ConfChange log.
	// b. 2 is isolated, 1 removes 2. When 2 rejoins the cluster, 2 will
	//  send stale MsgRequestVote to 1 and 3, at this time, we should tell 2 to gc itself.
	// c. 2 is isolated but can communicate with 3. 1 removes 3.
	//  2 will send stale MsgRequestVote to 3, 3 should ignore this message.
	// d. 2 is isolated but can communicate with 3. 1 removes 2, then adds 4, remove 3.
	//  2 will send stale MsgRequestVote to 3, 3 should tell 2 to gc itself.
	// e. 2 is isolated. 1 adds 4, 5, 6, removes 3, 1. Now assume 4 is leader.
	//  After 2 rejoins the cluster, 2 may send stale MsgRequestVote to 1 and 3,
	//  1 and 3 will ignore this message. Later 4 will send messages to 2 and 2 will
	//  rejoin the raft group again.
	// f. 2 is isolated. 1 adds 4, 5, 6, removes 3, 1. Now assume 4 is leader, and 4 removes 2.
	//  unlike case e, 2 will be stale forever.
	// TODO: for case f, if 2 is stale for a long time, 2 will communicate with scheduler and scheduler will
	// tell 2 is stale, so 2 can remove itself.
	region := d.Region()
	if util.IsEpochStale(fromEpoch, region.RegionEpoch) && util.FindPeer(region, fromStoreID) == nil {
		// The message is stale and not in current region.
		handleStaleMsg(d.ctx.trans, msg, region.RegionEpoch, isVoteMsg)
		return true
	}
	target := msg.GetToPeer()
	if target.Id < d.PeerId() {
		log.Infof("%s target peer ID %d is less than %d, msg maybe stale", d.Tag, target.Id, d.PeerId())
		return true
	} else if target.Id > d.PeerId() {
		if d.MaybeDestroy() {
			log.Infof("%s is stale as received a larger peer %s, destroying", d.Tag, target)
			d.destroyPeer()
			d.ctx.router.sendStore(message.NewMsg(message.MsgTypeStoreRaftMessage, msg))
		}
		return true
	}
	return false
}

func handleStaleMsg(trans Transport, msg *rspb.RaftMessage, curEpoch *metapb.RegionEpoch,
	needGC bool) {
	regionID := msg.RegionId
	fromPeer := msg.FromPeer
	toPeer := msg.ToPeer
	msgType := msg.Message.GetMsgType()

	if !needGC {
		log.Infof("[region %d] raft message %s is stale, current %v ignore it",
			regionID, msgType, curEpoch)
		return
	}
	gcMsg := &rspb.RaftMessage{
		RegionId:    regionID,
		FromPeer:    toPeer,
		ToPeer:      fromPeer,
		RegionEpoch: curEpoch,
		IsTombstone: true,
	}
	if err := trans.Send(gcMsg); err != nil {
		log.Errorf("[region %d] send message failed %v", regionID, err)
	}
}

func (d *peerMsgHandler) handleGCPeerMsg(msg *rspb.RaftMessage) {
	fromEpoch := msg.RegionEpoch
	if !util.IsEpochStale(d.Region().RegionEpoch, fromEpoch) {
		return
	}
	if !util.PeerEqual(d.Meta, msg.ToPeer) {
		log.Infof("%s receive stale gc msg, ignore", d.Tag)
		return
	}
	log.Infof("%s peer %s receives gc message, trying to remove", d.Tag, msg.ToPeer)
	if d.MaybeDestroy() {
		d.destroyPeer()
	}
}

// Returns `None` if the `msg` doesn't contain a snapshot or it contains a snapshot which
// doesn't conflict with any other snapshots or regions. Otherwise a `snap.SnapKey` is returned.
func (d *peerMsgHandler) checkSnapshot(msg *rspb.RaftMessage) (*snap.SnapKey, error) {
	if msg.Message.Snapshot == nil {
		return nil, nil
	}
	regionID := msg.RegionId
	snapshot := msg.Message.Snapshot
	key := snap.SnapKeyFromRegionSnap(regionID, snapshot)
	snapData := new(rspb.RaftSnapshotData)
	err := snapData.Unmarshal(snapshot.Data)
	if err != nil {
		return nil, err
	}
	snapRegion := snapData.Region
	peerID := msg.ToPeer.Id
	var contains bool
	for _, peer := range snapRegion.Peers {
		if peer.Id == peerID {
			contains = true
			break
		}
	}
	if !contains {
		log.Infof("%s %s doesn't contains peer %d, skip", d.Tag, snapRegion, peerID)
		return &key, nil
	}
	meta := d.ctx.storeMeta
	meta.Lock()
	defer meta.Unlock()
	if !util.RegionEqual(meta.regions[d.regionId], d.Region()) {
		if !d.isInitialized() {
			log.Infof("%s stale delegate detected, skip", d.Tag)
			return &key, nil
		} else {
			panic(fmt.Sprintf("%s meta corrupted %s != %s", d.Tag, meta.regions[d.regionId], d.Region()))
		}
	}

	existRegions := meta.getOverlapRegions(snapRegion)
	for _, existRegion := range existRegions {
		if existRegion.GetId() == snapRegion.GetId() {
			continue
		}
		log.Infof("%s region overlapped %s %s", d.Tag, existRegion, snapRegion)
		return &key, nil
	}

	// check if snapshot file exists.
	_, err = d.ctx.snapMgr.GetSnapshotForApplying(key)
	if err != nil {
		return nil, err
	}
	return nil, nil
}

func (d *peerMsgHandler) destroyPeer() {
	log.Infof("%s starts destroy", d.Tag)
	regionID := d.regionId
	// We can't destroy a peer which is applying snapshot.
	meta := d.ctx.storeMeta
	meta.Lock()
	defer meta.Unlock()
	isInitialized := d.isInitialized()
	if err := d.Destroy(d.ctx.engine, false); err != nil {
		// If not panic here, the peer will be recreated in the next restart,
		// then it will be gc again. But if some overlap region is created
		// before restarting, the gc action will delete the overlap region's
		// data too.
		panic(fmt.Sprintf("%s destroy peer %v", d.Tag, err))
	}
	d.ctx.router.close(regionID)
	d.stopped = true
	if isInitialized && meta.regionRanges.Delete(&regionItem{region: d.Region()}) == nil {
		panic(d.Tag + " meta corruption detected")
	}
	if _, ok := meta.regions[regionID]; !ok {
		panic(d.Tag + " meta corruption detected")
	}
	delete(meta.regions, regionID)
}

func (d *peerMsgHandler) findSiblingRegion() (result *metapb.Region) {
	meta := d.ctx.storeMeta
	meta.RLock()
	defer meta.RUnlock()
	item := &regionItem{region: d.Region()}
	meta.regionRanges.AscendGreaterOrEqual(item, func(i btree.Item) bool {
		result = i.(*regionItem).region
		return true
	})
	return
}

func (d *peerMsgHandler) onRaftGCLogTick() {
	d.ticker.schedule(PeerTickRaftLogGC)
	if !d.IsLeader() {
		return
	}

	appliedIdx := d.peerStorage.AppliedIndex()
	firstIdx, _ := d.peerStorage.FirstIndex()
	var compactIdx uint64
	if appliedIdx > firstIdx && appliedIdx-firstIdx >= d.ctx.cfg.RaftLogGcCountLimit {
		compactIdx = appliedIdx
	} else {
		return
	}

	y.Assert(compactIdx > 0)
	compactIdx -= 1
	if compactIdx < firstIdx {
		// In case compact_idx == first_idx before subtraction.
		return
	}

	term, err := d.RaftGroup.Raft.RaftLog.Term(compactIdx)
	if err != nil {
		log.Fatalf("appliedIdx: %d, firstIdx: %d, compactIdx: %d", appliedIdx, firstIdx, compactIdx)
		panic(err)
	}

	// Create a compact log request and notify directly.
	regionID := d.regionId
	request := newCompactLogRequest(regionID, d.Meta, compactIdx, term)
	d.proposeRaftCommand(request, nil)
}

func (d *peerMsgHandler) onSplitRegionCheckTick() {
	d.ticker.schedule(PeerTickSplitRegionCheck)
	// To avoid frequent scan, we only add new scan tasks if all previous tasks
	// have finished.
	if len(d.ctx.splitCheckTaskSender) > 0 {
		return
	}

	if !d.IsLeader() {
		return
	}
	if d.ApproximateSize != nil && d.SizeDiffHint < d.ctx.cfg.RegionSplitSize/8 {
		return
	}
	d.ctx.splitCheckTaskSender <- &runner.SplitCheckTask{
		Region: d.Region(),
	}
	d.SizeDiffHint = 0
}

func (d *peerMsgHandler) onPrepareSplitRegion(regionEpoch *metapb.RegionEpoch, splitKey []byte, cb *message.Callback) {
	if err := d.validateSplitRegion(regionEpoch, splitKey); err != nil {
		cb.Done(ErrResp(err))
		return
	}
	region := d.Region()
	d.ctx.schedulerTaskSender <- &runner.SchedulerAskSplitTask{
		Region:   region,
		SplitKey: splitKey,
		Peer:     d.Meta,
		Callback: cb,
	}
}

func (d *peerMsgHandler) validateSplitRegion(epoch *metapb.RegionEpoch, splitKey []byte) error {
	if len(splitKey) == 0 {
		err := errors.Errorf("%s split key should not be empty", d.Tag)
		log.Error(err)
		return err
	}

	if !d.IsLeader() {
		// region on this store is no longer leader, skipped.
		log.Infof("%s not leader, skip", d.Tag)
		return &util.ErrNotLeader{
			RegionId: d.regionId,
			Leader:   d.getPeerFromCache(d.LeaderId()),
		}
	}

	region := d.Region()
	latestEpoch := region.GetRegionEpoch()

	// This is a little difference for `check_region_epoch` in region split case.
	// Here we just need to check `version` because `conf_ver` will be update
	// to the latest value of the peer, and then send to Scheduler.
	if latestEpoch.Version != epoch.Version {
		log.Infof("%s epoch changed, retry later, prev_epoch: %s, epoch %s",
			d.Tag, latestEpoch, epoch)
		return &util.ErrEpochNotMatch{
			Message: fmt.Sprintf("%s epoch changed %s != %s, retry later", d.Tag, latestEpoch, epoch),
			Regions: []*metapb.Region{region},
		}
	}
	return nil
}

func (d *peerMsgHandler) onApproximateRegionSize(size uint64) {
	d.ApproximateSize = &size
}

func (d *peerMsgHandler) onSchedulerHeartbeatTick() {
	d.ticker.schedule(PeerTickSchedulerHeartbeat)

	if !d.IsLeader() {
		return
	}
	d.HeartbeatScheduler(d.ctx.schedulerTaskSender)
}

func (d *peerMsgHandler) onGCSnap(snaps []snap.SnapKeyWithSending) {
	compactedIdx := d.peerStorage.truncatedIndex()
	compactedTerm := d.peerStorage.truncatedTerm()
	for _, snapKeyWithSending := range snaps {
		key := snapKeyWithSending.SnapKey
		if snapKeyWithSending.IsSending {
			snap, err := d.ctx.snapMgr.GetSnapshotForSending(key)
			if err != nil {
				log.Errorf("%s failed to load snapshot for %s %v", d.Tag, key, err)
				continue
			}
			if key.Term < compactedTerm || key.Index < compactedIdx {
				log.Infof("%s snap file %s has been compacted, delete", d.Tag, key)
				d.ctx.snapMgr.DeleteSnapshot(key, snap, false)
			} else if fi, err1 := snap.Meta(); err1 == nil {
				modTime := fi.ModTime()
				if time.Since(modTime) > 4*time.Hour {
					log.Infof("%s snap file %s has been expired, delete", d.Tag, key)
					d.ctx.snapMgr.DeleteSnapshot(key, snap, false)
				}
			}
		} else if key.Term <= compactedTerm &&
			(key.Index < compactedIdx || key.Index == compactedIdx) {
			log.Infof("%s snap file %s has been applied, delete", d.Tag, key)
			a, err := d.ctx.snapMgr.GetSnapshotForApplying(key)
			if err != nil {
				log.Errorf("%s failed to load snapshot for %s %v", d.Tag, key, err)
				continue
			}
			d.ctx.snapMgr.DeleteSnapshot(key, a, false)
		}
	}
}

func newAdminRequest(regionID uint64, peer *metapb.Peer) *raft_cmdpb.RaftCmdRequest {
	return &raft_cmdpb.RaftCmdRequest{
		Header: &raft_cmdpb.RaftRequestHeader{
			RegionId: regionID,
			Peer:     peer,
		},
	}
}

func newCompactLogRequest(regionID uint64, peer *metapb.Peer, compactIndex, compactTerm uint64) *raft_cmdpb.RaftCmdRequest {
	req := newAdminRequest(regionID, peer)
	req.AdminRequest = &raft_cmdpb.AdminRequest{
		CmdType: raft_cmdpb.AdminCmdType_CompactLog,
		CompactLog: &raft_cmdpb.CompactLogRequest{
			CompactIndex: compactIndex,
			CompactTerm:  compactTerm,
		},
	}
	return req
}
