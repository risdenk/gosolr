// Copyright 2016 FullStory, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package smservice

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/fullstorydev/gosolr/solrman/solrmanapi"
	"github.com/fullstorydev/gosolr/solrmonitor"
)

// Long call; run in a go routine. Run the given split operation and records the result when done.
func (s *SolrManService) runSplitOperation(split *solrmanapi.OpRecord) {
	err := s.doRunSplitOperation(split)
	if err != nil {
		split.Error = fmt.Sprintf("failed SplitShard request: %s with err: %s", split, err)
		split.FinishedMs = nowMillis()
		s.Logger.Errorf("failed SplitShard request: %+v with err: %s", split, err)
	} else {
		split.FinishedMs = nowMillis()
		s.Logger.Infof("completed SplitShard request: %s", split)
	}

	func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.inProgressOps, split.Key())
	}()

	conn := s.RedisPool.Get()
	defer conn.Close()
	if _, err := conn.Do("HDEL", OpMapRedisKey, split.Key()); err != nil {
		s.Logger.Warningf("failed to HDEL split %s from redis: %s", split.Key(), err)
	}
	if _, err := conn.Do("LPUSH", CompletedOpRedisKey, jsonString(split)); err != nil {
		s.Logger.Warningf("failed to LPUSH completed move %s to redis: %s", split.Key(), err)
	}
	if _, err := conn.Do("LTRIM", CompletedOpRedisKey, 0, 99); err != nil {
		s.Logger.Warningf("failed to LTRIM %s: %s", CompletedOpRedisKey, err)
	}
}

// Run the given split operation; returns an error, or nil if it succeeds.
func (s *SolrManService) doRunSplitOperation(split *solrmanapi.OpRecord) error {
	s.Logger.Infof("splitting shard %q of collection %q", split.Shard, split.Collection)

	// Fail if we can't retrieve the initial state.
	coll, err := s.SolrMonitor.GetCollectionState(split.Collection)
	if err != nil {
		return err
	}

	if coll == nil {
		return cherrf(err, "no such collection (maybe it disappeared?)")
	}

	if _, ok := coll.Shards[split.Shard]; !ok { // guard against no-such-shard
		return cherrf(err, "no such shard %s in collection %s", split.Shard, split.Collection)
	}

	// this must be the initial request, so the first step is to start a SPLITSHARD command
	requestId := newSolrRequestId()
	if err := s.solrClient.SplitShard(split.Collection, split.Shard, requestId); err != nil {
		return cherrf(err, "failed to issue SPLITSHARD command")
	}
	s.Logger.Debugf("async SPLITSHARD command issued successfully (requestid = %q)", requestId)

	// Wait on the split to happen
	for {
		// Only check request status to determine error conditions.
		_, errMsg, err := checkRequestStatus(s.Logger, "SPLITSHARD", requestId, s.solrClient)
		if err != nil {
			return cherrf(err, "failed to get status of request %q", requestId)
		} else if errMsg != "" {
			return errorf("async SPLITSHARD failed: %s", errMsg)
		}

		// Track the split ourselves, because solr is not reliable at reporting async results.
		// else (isDone = false) we check manually to see if the split has completed because solrcloud is not reliable
		// about reporting when async commands are done (it will sometimes report commands as in progress when they have
		// been done for a long time)
		coll, err := s.SolrMonitor.GetCollectionState(split.Collection)
		if err != nil {
			return cherrf(err, "failed to read cluster state")
		}

		child0 := split.Shard + "_0"
		child1 := split.Shard + "_1"
		if activeShardExists(coll, child0) && activeShardExists(coll, child1) && inactiveShardExists(coll, split.Shard) {
			s.Logger.Debugf("shards %s and %s exist and are active, and shard %s is inactive - assuming SPLITSHARD has completed", child0, child1, split.Shard)
			break
		}

		// consider something event-driven instead of polling
		time.Sleep(10 * time.Second)
	}

	// Finally we delete the old shard.
	if err := s.solrClient.DeleteShard(split.Collection, split.Shard); err != nil {
		// If this fails because the shard doesn't exist, then we assume that some prior attempt finished the job
		// (probably the http request timed out, but the actual api action eventually succeeded within Solr).  So we
		// just treat this as a success.
		if solrErr := isNoSuchShardError(err, split.Collection, split.Shard); solrErr != nil {
			s.Logger.Debugf("assuming shard was previously deleted: %s", solrErr)
			return nil
		} else {
			return cherrf(err, "failed to issue DELETESHARD command")
		}
	}
	s.Logger.Infof("deleted shard %q of collection %q", split.Shard, split.Collection)
	return nil
}

func activeShardExists(coll *solrmonitor.CollectionState, target string) bool {
	for name, state := range coll.Shards {
		if name == target && state.IsActive() {
			return true
		}
	}
	return false
}

func inactiveShardExists(coll *solrmonitor.CollectionState, target string) bool {
	for name, state := range coll.Shards {
		if name == target && !state.IsActive() {
			return true
		}
	}
	return false
}

func isNoSuchShardError(err error, collName, shardName string) *ErrorRsp {
	if chErr, ok := err.(*chainedError); ok {
		err = chErr.Root()
	}

	if err, ok := err.(*ErrorRsp); ok {
		s := fmt.Sprintf("No shard with name %s exists for collection %s", shardName, collName)
		if err.Code == http.StatusBadRequest && strings.Contains(err.Msg, s) {
			return err
		}
	}

	return nil
}
