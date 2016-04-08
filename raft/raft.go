package raft

import (
	"errors"
	"time"
)

func (s *Server) run() {
	state := s.State()
	for state != Stopped {
		select {
		case <-s.stopCh:
			s.setState(Stopped)
			return
		default:
		}

		switch s.State() {
		case Follower:
			s.runAsFollower()
		case Candidate:
			s.runAsCandidate()
		case Leader:
			s.runAsLeader()
		}
		state = s.State()
	}
}

func (s *Server) runAsFollower() {
	s.debug("server.state: %s enter %s", s.LocalAddress(), s.State().String())

	electionTimeout := time.NewTimer(randomDuration(DefaultElectionTimeout))

	for s.State() == Follower {
		select {
		case rpc := <-s.rpcCh:
			s.processRPC(rpc)
			electionTimeout.Reset(randomDuration(DefaultElectionTimeout))
		case <-electionTimeout.C:
			s.setState(Candidate)
			electionTimeout.Reset(randomDuration(DefaultElectionTimeout))
		case <-s.stopCh:
			electionTimeout.Stop()
			s.setState(Stopped)
			return
		}
	}
}

func (s *Server) runAsCandidate() {
	s.debug("server.state: %s enter %s", s.LocalAddress(), s.State().String())
	doVote := true
	votesGranted := 0
	var electionTimeout *time.Timer
	var respCh chan *RequestVoteResponse

	for s.State() == Candidate {
		if doVote {
			s.currentTerm++
			s.votedFor = s.LocalAddress()
			respCh = make(chan *RequestVoteResponse, len(s.peers))
			for _, peer := range s.peers {
				go func(peer string) {
					s.sendVoteRequest(peer, newRequestVoteRequest(s.currentTerm, s.LocalAddress(), 1, 0), respCh)
				}(peer)
			}
			votesGranted = 1
			electionTimeout = time.NewTimer(randomDuration(DefaultElectionTimeout))
			doVote = false
		}
		// If receive enough vote, stop waiting and promote to leader
		if votesGranted == s.QuorumSize() {
			s.setState(Leader)
			s.debug("server.state: %s become %s", s.LocalAddress(), s.State().String())
			return
		}

		select {
		case <-s.stopCh:
			electionTimeout.Stop()
			s.setState(Stopped)
			return
		case resp := <-respCh:
			if success := s.processRequestVoteResponse(resp); success {
				votesGranted++
			}
		case rpc := <-s.rpcCh:
			s.processRPC(rpc)
		case <-electionTimeout.C:
			doVote = true
		}
	}
}

func (s *Server) runAsLeader() {
	s.debug("server.state: %s as %s", s.LocalAddress(), s.State().String())
	s.followers = make(map[string]*follower)
	s.applying = make(map[uint64]*Log)
	s.applyCh = make(chan *Log)
	s.commitCh = make(chan *Log)
	// send heartbeat to notify leadership
	for _, peer := range s.peers {
		s.startReplication(peer)
	}

	for s.State() == Leader {
		select {
		case <-s.stopCh:
			for _, f := range s.followers {
				close(f.stopCh)
			}
			return
		case rpc := <-s.rpcCh:
			s.processRPC(rpc)
		case newLog := <-s.applyCh:
			s.dispatchLog(newLog)
		case commitLog := <-s.commitCh:
			// TODO: process log
			s.debug("server.log.commit: index %d", commitLog.Index)
			s.setCommitIndex(commitLog.Index)
		}
	}
}

func (s *Server) dispatchLog(applyLog *Log) {
	currentTerm := s.Term()
	lastLogIndex := s.LastLogIndex()

	applyLog.Term = currentTerm
	applyLog.Index = lastLogIndex + 1
	applyLog.majorityQuorum = s.QuorumSize()
	applyLog.count = 0

	if err := s.logs.SetLog(applyLog); err != nil {
		return
	}

	s.applying[applyLog.Index] = applyLog

	s.setLastLog(lastLogIndex+1, currentTerm)
	for _, f := range s.followers {
		asyncNotifyCh(f.replicateCh)
	}
}

func (s *Server) startReplication(peer string) {
	lastLogIndex := s.LastLogIndex()
	f := &follower{
		peer:        peer,
		currentTerm: s.Term(),
		matchIndex:  0,
		nextIndex:   lastLogIndex + 1,
		replicateCh: make(chan struct{}),
		stopCh:      make(chan bool),
	}

	s.followers[peer] = f
	go s.replicate(f)
}

func (s *Server) processRPC(rpc RPC) {
	s.debug("server.process.rpc %s : %+v", s.LocalAddress(), rpc)
	switch cmd := rpc.Command.(type) {
	case *AppendEntryRequest:
		// s.debug("server.entry.append.received: %s %+v", s.LocalAddress(), cmd)
		s.processAppendEntries(rpc, cmd)
	case *RequestVoteRequest:
		// s.debug("server.vote.request.received %s %+v", s.LocalAddress(), cmd)
		s.processRequestVote(rpc, cmd)
	default:
		s.err("server.command.error: unexpected command: %v", rpc.Command)
		rpc.Response(nil, errors.New("Unxepected Command"))
	}
}

func (s *Server) processAppendEntries(rpc RPC, req *AppendEntryRequest) {
	resp := &AppendEntryResponse{
		Term:         s.Term(),
		LastLogIndex: s.LastLogIndex(),
		Success:      false,
	}

	var err error
	defer func() {
		s.debug("server.entry.append.response: %+v", resp)
		rpc.Response(resp, err)
	}()

	if req.Term < s.Term() {
		return
	}

	if req.Term > s.Term() || s.State() != Follower {
		s.setTerm(req.Term)
		s.setState(Follower)
		resp.Term = req.Term
	}
	s.setLeader(req.Leader)

	lastLogIndex, lastLogTerm := s.LastLog()

	var prevLogTerm uint64

	if req.PrevLogIndex == lastLogIndex {
		prevLogTerm = lastLogTerm
	} else {
		s.debug("prevLogIdx: %d", req.PrevLogIndex)
		prevLog, err := s.logs.GetLog(req.PrevLogIndex)
		if err != nil {
			s.debug("failed to get previous log: %v %s (last %v)", req.PrevLogIndex, err, lastLogIndex)
			return
		}
		prevLogTerm = prevLog.Term
	}

	if req.PrevLogTerm != prevLogTerm {
		s.debug("server.entry.append: Previouse log term mis-match: current: %v request: %v", prevLogTerm, req.PrevLogTerm)
		return
	}

	// Process any new entry
	if n := len(req.Entries); n > 0 {
		first := req.Entries[0]
		last := req.Entries[n-1]

		lastLogIndex := s.LastLogIndex()
		if first.Index <= lastLogIndex {
			s.debug("server.log.clear: from %d to %d", first.Index, lastLogIndex)
			if err := s.logs.DeleteRange(first.Index, lastLogIndex); err != nil {
				s.debug("server.logs.clear: Failed: from %d to %d. Error  %v", first.Index, lastLogIndex, err)
				return
			}
		}

		if err := s.logs.SetLogs(req.Entries); err != nil {
			s.debug("server.logs.append.failed: %v", err)
			return
		}

		s.setLastLog(last.Index, last.Term)
		resp.LastLogIndex = s.LastLogIndex()
		// s.debug("server.entry.append: LastLogIndex: %v LastLogTerm: %v", last.Index, last.Term)
	}

	// Update commit index
	if req.LeaderCommitIndex > s.CommitIndex() {
		idx := min(req.LeaderCommitIndex, s.LastLogIndex())
		s.setCommitIndex(idx)
		s.debug("server.commit.index: %v", s.CommitIndex())
		// TODO: process log
	}

	resp.Success = true
	return
}

func (s *Server) sendVoteRequest(peer string, req *RequestVoteRequest, c chan *RequestVoteResponse) {
	s.debug("server.vote.request: %s -> %s [%+v]", s.LocalAddress(), peer, req)
	if resp := s.Transport().RequestVote(peer, req); resp != nil {
		c <- resp
	}
}

func (s *Server) processRequestVote(rpc RPC, req *RequestVoteRequest) {
	resp := &RequestVoteResponse{
		Term:        s.Term(),
		VoteGranted: false,
	}

	var err error
	defer func() {
		s.debug("server.vote.response: %+v", resp)
		rpc.Response(resp, err)
	}()

	// If term of request smaller than current term, reject
	if req.Term < s.Term() {
		return
	}

	// If term of request larger than current term, update current term
	// If term is equal but already voted for different candidate then
	// don't vote for this candidate
	if req.Term > s.Term() {
		s.setTerm(req.Term)
		resp.Term = s.Term()
	} else if s.votedFor != "" && s.votedFor != req.CandidateName {
		s.debug("server.vote.duplicate: %s already vote for %s", req.CandidateName, s.votedFor)
		return
	}

	// If the candidate's log is not update-to-date, don't vote
	lastIndex, lastTerm := s.LastLog()
	s.debug("server.log.last: %s [Index: %v/Term: %v]", s.LocalAddress(), lastIndex, lastTerm)
	if lastIndex > req.LastLogIndex || lastTerm > req.LastLogTerm {
		s.debug("server.log.outdate: current: [Index: %v,Term: %v] : request: [Index: %v,Term: %v]", lastIndex,
			lastTerm, req.LastLogIndex, req.LastLogTerm)
		return
	}

	// If everything ok then vote
	s.votedFor = req.CandidateName
	resp.VoteGranted = true
	resp.Term = s.Term()
	return
}

func (s *Server) processRequestVoteResponse(resp *RequestVoteResponse) bool {
	if resp.VoteGranted && resp.Term == s.currentTerm {
		return true
	}
	if resp.Term > s.currentTerm {
		s.setTerm(resp.Term)
		s.setState(Follower)
	}

	return false
}