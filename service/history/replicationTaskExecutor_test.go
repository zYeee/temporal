// Copyright (c) 2020 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package history

import (
	"fmt"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/pborman/uuid"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"github.com/uber-go/tally"

	commonproto "go.temporal.io/temporal-proto/common"
	"go.temporal.io/temporal-proto/enums"

	"github.com/temporalio/temporal/.gen/go/history"
	"github.com/temporalio/temporal/.gen/go/history/historyservicetest"
	"github.com/temporalio/temporal/.gen/go/shared"
	"github.com/temporalio/temporal/.gen/proto/adminservicemock"
	"github.com/temporalio/temporal/.gen/proto/persistenceblobs"
	"github.com/temporalio/temporal/client"
	"github.com/temporalio/temporal/common"
	"github.com/temporalio/temporal/common/cache"
	"github.com/temporalio/temporal/common/cluster"
	"github.com/temporalio/temporal/common/log"
	"github.com/temporalio/temporal/common/metrics"
	"github.com/temporalio/temporal/common/mocks"
	"github.com/temporalio/temporal/common/persistence"
	"github.com/temporalio/temporal/common/resource"
	"github.com/temporalio/temporal/common/xdc"
)

type (
	replicationTaskExecutorSuite struct {
		suite.Suite
		*require.Assertions
		controller *gomock.Controller

		currentCluster      string
		mockResource        *resource.Test
		mockShard           ShardContext
		mockEngine          *MockEngine
		config              *Config
		historyClient       *historyservicetest.MockClient
		mockDomainCache     *cache.MockDomainCache
		mockClientBean      *client.MockBean
		adminClient         *adminservicemock.MockAdminServiceClient
		clusterMetadata     *cluster.MockMetadata
		executionManager    *mocks.ExecutionManager
		nDCHistoryResender  *xdc.MockNDCHistoryResender
		historyRereplicator *xdc.MockHistoryRereplicator

		replicationTaskHandler *replicationTaskExecutorImpl
	}
)

func TestReplicationTaskExecutorSuite(t *testing.T) {
	s := new(replicationTaskExecutorSuite)
	suite.Run(t, s)
}

func (s *replicationTaskExecutorSuite) SetupSuite() {

}

func (s *replicationTaskExecutorSuite) TearDownSuite() {

}

func (s *replicationTaskExecutorSuite) SetupTest() {
	s.Assertions = require.New(s.T())
	s.controller = gomock.NewController(s.T())
	s.currentCluster = "test"

	s.mockResource = resource.NewTest(s.controller, metrics.History)
	s.mockDomainCache = s.mockResource.DomainCache
	s.mockClientBean = s.mockResource.ClientBean
	s.adminClient = s.mockResource.RemoteAdminClient
	s.clusterMetadata = s.mockResource.ClusterMetadata
	s.executionManager = s.mockResource.ExecutionMgr
	s.nDCHistoryResender = xdc.NewMockNDCHistoryResender(s.controller)
	s.historyRereplicator = &xdc.MockHistoryRereplicator{}
	logger := log.NewNoop()
	s.mockShard = &shardContextImpl{
		shardID:  0,
		Resource: s.mockResource,
		shardInfo: &persistence.ShardInfoWithFailover{ShardInfo: &persistenceblobs.ShardInfo{
			ShardID:                0,
			RangeID:                1,
			ReplicationAckLevel:    0,
			ReplicationDLQAckLevel: map[string]int64{"test": -1},
		}},
		transferSequenceNumber:    1,
		maxTransferSequenceNumber: 100000,
		closeCh:                   make(chan int, 100),
		config:                    NewDynamicConfigForTest(),
		logger:                    logger,
		remoteClusterCurrentTime:  make(map[string]time.Time),
		executionManager:          s.executionManager,
	}
	s.mockEngine = NewMockEngine(s.controller)
	s.config = NewDynamicConfigForTest()
	s.historyClient = historyservicetest.NewMockClient(s.controller)
	metricsClient := metrics.NewClient(tally.NoopScope, metrics.History)
	s.clusterMetadata.EXPECT().GetCurrentClusterName().Return("active").AnyTimes()

	s.replicationTaskHandler = newReplicationTaskExecutor(
		s.currentCluster,
		s.mockDomainCache,
		s.nDCHistoryResender,
		s.historyRereplicator,
		s.mockEngine,
		metricsClient,
		s.mockShard.GetLogger(),
	).(*replicationTaskExecutorImpl)
}

func (s *replicationTaskExecutorSuite) TearDownTest() {
	s.controller.Finish()
	s.mockResource.Finish(s.T())
}

func (s *replicationTaskExecutorSuite) TestConvertRetryTaskError_OK() {
	err := &shared.RetryTaskError{}
	_, ok := s.replicationTaskHandler.convertRetryTaskError(err)
	s.True(ok)
}

func (s *replicationTaskExecutorSuite) TestConvertRetryTaskError_NotOK() {
	err := &shared.RetryTaskV2Error{}
	_, ok := s.replicationTaskHandler.convertRetryTaskError(err)
	s.False(ok)
}

func (s *replicationTaskExecutorSuite) TestConvertRetryTaskV2Error_OK() {
	err := &shared.RetryTaskV2Error{}
	_, ok := s.replicationTaskHandler.convertRetryTaskV2Error(err)
	s.True(ok)
}

func (s *replicationTaskExecutorSuite) TestConvertRetryTaskV2Error_NotOK() {
	err := &shared.RetryTaskError{}
	_, ok := s.replicationTaskHandler.convertRetryTaskV2Error(err)
	s.False(ok)
}

func (s *replicationTaskExecutorSuite) TestFilterTask() {
	domainID := uuid.New()
	s.mockDomainCache.EXPECT().
		GetDomainByID(domainID).
		Return(cache.NewGlobalDomainCacheEntryForTest(
			nil,
			nil,
			&persistence.DomainReplicationConfig{
				Clusters: []*persistence.ClusterReplicationConfig{
					{
						ClusterName: "test",
					},
				}},
			0,
			s.clusterMetadata,
		), nil)
	ok, err := s.replicationTaskHandler.filterTask(domainID, false)
	s.NoError(err)
	s.True(ok)
}

func (s *replicationTaskExecutorSuite) TestFilterTask_Error() {
	domainID := uuid.New()
	s.mockDomainCache.EXPECT().
		GetDomainByID(domainID).
		Return(nil, fmt.Errorf("test"))
	ok, err := s.replicationTaskHandler.filterTask(domainID, false)
	s.Error(err)
	s.False(ok)
}

func (s *replicationTaskExecutorSuite) TestFilterTask_EnforceApply() {
	domainID := uuid.New()
	ok, err := s.replicationTaskHandler.filterTask(domainID, true)
	s.NoError(err)
	s.True(ok)
}

func (s *replicationTaskExecutorSuite) TestProcessTaskOnce_SyncActivityReplicationTask() {
	domainID := uuid.New()
	workflowID := uuid.New()
	runID := uuid.New()
	task := &commonproto.ReplicationTask{
		TaskType: enums.ReplicationTaskTypeSyncActivity,
		Attributes: &commonproto.ReplicationTask_SyncActivityTaskAttributes{
			SyncActivityTaskAttributes: &commonproto.SyncActivityTaskAttributes{
				DomainId:   domainID,
				WorkflowId: workflowID,
				RunId:      runID,
			},
		},
	}
	request := &history.SyncActivityRequest{
		DomainId:           common.StringPtr(domainID),
		WorkflowId:         common.StringPtr(workflowID),
		RunId:              common.StringPtr(runID),
		Version:            common.Int64Ptr(0),
		ScheduledId:        common.Int64Ptr(0),
		ScheduledTime:      common.Int64Ptr(0),
		StartedId:          common.Int64Ptr(0),
		StartedTime:        common.Int64Ptr(0),
		LastHeartbeatTime:  common.Int64Ptr(0),
		Attempt:            common.Int32Ptr(0),
		LastFailureReason:  common.StringPtr(""),
		LastWorkerIdentity: common.StringPtr(""),
	}

	s.mockEngine.EXPECT().SyncActivity(gomock.Any(), request).Return(nil).Times(1)
	_, err := s.replicationTaskHandler.execute(s.currentCluster, task, true)
	s.NoError(err)
}

func (s *replicationTaskExecutorSuite) TestProcessTaskOnce_HistoryReplicationTask() {
	domainID := uuid.New()
	workflowID := uuid.New()
	runID := uuid.New()
	task := &commonproto.ReplicationTask{
		TaskType: enums.ReplicationTaskTypeHistory,
		Attributes: &commonproto.ReplicationTask_HistoryTaskAttributes{
			HistoryTaskAttributes: &commonproto.HistoryTaskAttributes{
				DomainId:     domainID,
				WorkflowId:   workflowID,
				RunId:        runID,
				FirstEventId: 1,
				NextEventId:  5,
				Version:      common.EmptyVersion,
			},
		},
	}
	request := &history.ReplicateEventsRequest{
		DomainUUID: common.StringPtr(domainID),
		WorkflowExecution: &shared.WorkflowExecution{
			WorkflowId: common.StringPtr(workflowID),
			RunId:      common.StringPtr(runID),
		},
		SourceCluster:     common.StringPtr("test"),
		ForceBufferEvents: common.BoolPtr(false),
		FirstEventId:      common.Int64Ptr(1),
		NextEventId:       common.Int64Ptr(5),
		Version:           common.Int64Ptr(common.EmptyVersion),
		ResetWorkflow:     common.BoolPtr(false),
		NewRunNDC:         common.BoolPtr(false),
	}

	s.mockEngine.EXPECT().ReplicateEvents(gomock.Any(), request).Return(nil).Times(1)
	_, err := s.replicationTaskHandler.execute(s.currentCluster, task, true)
	s.NoError(err)
}

func (s *replicationTaskExecutorSuite) TestProcess_HistoryV2ReplicationTask() {
	domainID := uuid.New()
	workflowID := uuid.New()
	runID := uuid.New()
	task := &commonproto.ReplicationTask{
		TaskType: enums.ReplicationTaskTypeHistoryV2,
		Attributes: &commonproto.ReplicationTask_HistoryTaskV2Attributes{
			HistoryTaskV2Attributes: &commonproto.HistoryTaskV2Attributes{
				DomainId:   domainID,
				WorkflowId: workflowID,
				RunId:      runID,
			},
		},
	}
	request := &history.ReplicateEventsV2Request{
		DomainUUID: common.StringPtr(domainID),
		WorkflowExecution: &shared.WorkflowExecution{
			WorkflowId: common.StringPtr(workflowID),
			RunId:      common.StringPtr(runID),
		},
	}

	s.mockEngine.EXPECT().ReplicateEventsV2(gomock.Any(), request).Return(nil).Times(1)
	_, err := s.replicationTaskHandler.execute(s.currentCluster, task, true)
	s.NoError(err)
}
