package rest_test

import (
	"net/http"

	"github.com/Gthulhu/api/config"
	"github.com/Gthulhu/api/manager/domain"
	"github.com/Gthulhu/api/manager/rest"
	"github.com/stretchr/testify/mock"
)

func (suite *HandlerTestSuite) TestInternalStrategyUpsertCreatesIntent() {
	suite.T().Setenv("MANAGER_INTERNAL_TOKEN", "controller-test-token")
	strategyReq := rest.CreateScheduleStrategyRequest{
		StrategyNamespace: "aether-smf-autoscaling",
		K8sNamespace:      []string{"aether-5gc"},
		LabelSelectors: []rest.LabelSelector{
			{Key: "app", Value: "smf"},
		},
		Priority:      2,
		ExecutionTime: 50000000,
	}

	suite.MockK8SAdapter.EXPECT().QueryPods(mock.Anything, mock.Anything).Return([]*domain.Pod{{PodID: "smf-pod", Name: "smf-0", K8SNamespace: "aether-5gc", Labels: map[string]string{"app": "smf"}, NodeID: "node1"}}, nil).Once()
	suite.MockK8SAdapter.EXPECT().QueryDecisionMakerPods(mock.Anything, mock.Anything).Return([]*domain.DecisionMakerPod{{Host: "dm-host", NodeID: "node1", Port: 8080}}, nil).Once()
	suite.MockDMAdapter.EXPECT().SendSchedulingIntent(mock.Anything, mock.Anything, mock.Anything).Return(nil).Once()

	response := rest.SuccessResponse[rest.InternalScheduleStrategyResponse]{}
	_, recorder := suite.sendV1Request(http.MethodPut, "/internal/strategies", &strategyReq, &response, "controller-test-token")
	suite.Require().Equal(http.StatusOK, recorder.Code)
	suite.Require().NotNil(response.Data)
	suite.Require().NotEmpty(response.Data.StrategyID)

	intents := &domain.QueryIntentOptions{PodIDs: []string{"smf-pod"}}
	suite.Require().NoError(suite.Handler.Svc.ListScheduleIntents(suite.Ctx, intents))
	suite.Require().Len(intents.Result, 1)
	suite.Require().Equal(strategyReq.Priority, intents.Result[0].Priority)
	suite.Require().Equal(strategyReq.ExecutionTime, intents.Result[0].ExecutionTime)

	baselineReq := strategyReq
	baselineReq.Priority = 10
	baselineReq.ExecutionTime = 20000000
	suite.MockK8SAdapter.EXPECT().QueryPods(mock.Anything, mock.Anything).Return([]*domain.Pod{{PodID: "smf-pod", Name: "smf-0", K8SNamespace: "aether-5gc", Labels: map[string]string{"app": "smf"}, NodeID: "node1"}}, nil).Once()
	suite.MockK8SAdapter.EXPECT().QueryDecisionMakerPods(mock.Anything, mock.Anything).Return([]*domain.DecisionMakerPod{{Host: "dm-host", NodeID: "node1", Port: 8080}}, nil).Twice()
	suite.MockDMAdapter.EXPECT().DeleteSchedulingIntents(mock.Anything, mock.Anything, mock.Anything).Return(nil).Once()
	suite.MockDMAdapter.EXPECT().SendSchedulingIntent(mock.Anything, mock.Anything, mock.Anything).Return(nil).Once()

	updateResponse := rest.SuccessResponse[rest.InternalScheduleStrategyResponse]{}
	_, recorder = suite.sendV1Request(http.MethodPut, "/internal/strategies", &baselineReq, &updateResponse, "controller-test-token")
	suite.Require().Equal(http.StatusOK, recorder.Code)
	suite.Require().Equal(response.Data.StrategyID, updateResponse.Data.StrategyID)

	updatedIntents := &domain.QueryIntentOptions{PodIDs: []string{"smf-pod"}}
	suite.Require().NoError(suite.Handler.Svc.ListScheduleIntents(suite.Ctx, updatedIntents))
	suite.Require().Len(updatedIntents.Result, 1)
	suite.Require().Equal(baselineReq.Priority, updatedIntents.Result[0].Priority)
	suite.Require().Equal(baselineReq.ExecutionTime, updatedIntents.Result[0].ExecutionTime)
}

func (suite *HandlerTestSuite) TestInternalStrategyUpsertRejectsInvalidToken() {
	suite.T().Setenv("MANAGER_INTERNAL_TOKEN", "controller-test-token")
	response := rest.ErrorResponse{}
	_, recorder := suite.sendV1Request(http.MethodPut, "/internal/strategies", &rest.CreateScheduleStrategyRequest{StrategyNamespace: "aether-smf-autoscaling"}, &response, "wrong-token")
	suite.Require().Equal(http.StatusUnauthorized, recorder.Code)
}

func (suite *HandlerTestSuite) TestIntegrationStrategyHandler() {
	adminUser, adminPwd := config.GetManagerConfig().Account.AdminEmail, config.GetManagerConfig().Account.AdminPassword
	adminToken := suite.login(adminUser, adminPwd.Value(), http.StatusOK)

	strategyReq := rest.CreateScheduleStrategyRequest{
		LabelSelectors: []rest.LabelSelector{
			{
				Key: "test", Value: "test",
			},
		},
		Priority:      100,
		ExecutionTime: 100,
	}

	suite.MockK8SAdapter.EXPECT().QueryPods(mock.Anything, mock.Anything).Return([]*domain.Pod{{PodID: "Test", Labels: map[string]string{"test": "test"}, NodeID: "test"}}, nil).Once()
	suite.MockK8SAdapter.EXPECT().QueryDecisionMakerPods(mock.Anything, mock.Anything).Return([]*domain.DecisionMakerPod{{Host: "dm-host", NodeID: "test", Port: 8080}}, nil).Once()
	suite.MockDMAdapter.EXPECT().SendSchedulingIntent(mock.Anything, mock.Anything, mock.Anything).Return(nil).Times(1)
	suite.createStrategy(adminToken, &strategyReq, http.StatusOK)

	strategies := suite.listSelfStrategies(adminToken, http.StatusOK)
	suite.Require().Len(strategies.Strategies, 1, "Expected one strategy")
	suite.Require().Equal(strategyReq.LabelSelectors[0].Key, strategies.Strategies[0].LabelSelectors[0].Key, "Label selector key mismatch")
	suite.Require().Equal(strategyReq.LabelSelectors[0].Value, strategies.Strategies[0].LabelSelectors[0].Value, "Label selector value mismatch")
	suite.Require().Equal(strategyReq.Priority, strategies.Strategies[0].Priority, "Priority mismatch")
	suite.Require().Equal(strategyReq.ExecutionTime, strategies.Strategies[0].ExecutionTime, "ExecutionTime mismatch")

	intents := suite.listSelfIntents(adminToken, http.StatusOK)
	suite.Require().Len(intents.Intents, 1, "Expected one intent")
	suite.Require().Equal("Test", intents.Intents[0].PodID, "PodID mismatch")
	suite.Require().Equal(strategies.Strategies[0].ID.String(), intents.Intents[0].StrategyID.String(), "StrategyID mismatch")
	suite.Require().Equal(domain.IntentStateSent, intents.Intents[0].State, "State mismatch")
	suite.Require().Equal(strategyReq.Priority, intents.Intents[0].Priority, "Priority mismatch")
	suite.Require().Equal(strategyReq.ExecutionTime, intents.Intents[0].ExecutionTime, "ExecutionTime mismatch")
}

func (suite *HandlerTestSuite) TestIntegrationDeleteStrategyHandler() {
	adminUser, adminPwd := config.GetManagerConfig().Account.AdminEmail, config.GetManagerConfig().Account.AdminPassword
	adminToken := suite.login(adminUser, adminPwd.Value(), http.StatusOK)

	strategyReq := rest.CreateScheduleStrategyRequest{
		LabelSelectors: []rest.LabelSelector{
			{
				Key: "test", Value: "test",
			},
		},
		Priority:      100,
		ExecutionTime: 100,
	}

	// Create strategy
	suite.MockK8SAdapter.EXPECT().QueryPods(mock.Anything, mock.Anything).Return([]*domain.Pod{{PodID: "Test", Labels: map[string]string{"test": "test"}, NodeID: "test"}}, nil).Once()
	suite.MockK8SAdapter.EXPECT().QueryDecisionMakerPods(mock.Anything, mock.Anything).Return([]*domain.DecisionMakerPod{{Host: "dm-host", NodeID: "test", Port: 8080}}, nil).Once()
	suite.MockDMAdapter.EXPECT().SendSchedulingIntent(mock.Anything, mock.Anything, mock.Anything).Return(nil).Times(1)
	suite.createStrategy(adminToken, &strategyReq, http.StatusOK)

	strategies := suite.listSelfStrategies(adminToken, http.StatusOK)
	suite.Require().Len(strategies.Strategies, 1, "Expected one strategy")

	// Delete the strategy - need to mock DM notification
	suite.MockK8SAdapter.EXPECT().QueryDecisionMakerPods(mock.Anything, mock.Anything).Return([]*domain.DecisionMakerPod{{Host: "dm-host", NodeID: "test", Port: 8080}}, nil).Once()
	suite.MockDMAdapter.EXPECT().DeleteSchedulingIntents(mock.Anything, mock.Anything, mock.Anything).Return(nil).Once()
	suite.deleteStrategy(adminToken, strategies.Strategies[0].ID.Hex(), http.StatusOK)

	// Verify strategy and intents are deleted
	strategies = suite.listSelfStrategies(adminToken, http.StatusOK)
	suite.Require().Len(strategies.Strategies, 0, "Expected no strategies after deletion")

	intents := suite.listSelfIntents(adminToken, http.StatusOK)
	suite.Require().Len(intents.Intents, 0, "Expected no intents after strategy deletion")
}

func (suite *HandlerTestSuite) TestIntegrationDeleteIntentsHandler() {
	adminUser, adminPwd := config.GetManagerConfig().Account.AdminEmail, config.GetManagerConfig().Account.AdminPassword
	adminToken := suite.login(adminUser, adminPwd.Value(), http.StatusOK)

	strategyReq := rest.CreateScheduleStrategyRequest{
		LabelSelectors: []rest.LabelSelector{
			{
				Key: "test", Value: "test",
			},
		},
		Priority:      100,
		ExecutionTime: 100,
	}

	// Create strategy
	suite.MockK8SAdapter.EXPECT().QueryPods(mock.Anything, mock.Anything).Return([]*domain.Pod{{PodID: "Test1", Labels: map[string]string{"test": "test"}, NodeID: "test"}, {PodID: "Test2", Labels: map[string]string{"test": "test"}, NodeID: "test"}}, nil).Once()
	suite.MockK8SAdapter.EXPECT().QueryDecisionMakerPods(mock.Anything, mock.Anything).Return([]*domain.DecisionMakerPod{{Host: "dm-host", NodeID: "test", Port: 8080}}, nil).Once()
	suite.MockDMAdapter.EXPECT().SendSchedulingIntent(mock.Anything, mock.Anything, mock.Anything).Return(nil).Times(1)
	suite.createStrategy(adminToken, &strategyReq, http.StatusOK)

	intents := suite.listSelfIntents(adminToken, http.StatusOK)
	suite.Require().Len(intents.Intents, 2, "Expected two intents")

	// Delete one intent - need to mock DM notification
	suite.MockK8SAdapter.EXPECT().QueryDecisionMakerPods(mock.Anything, mock.Anything).Return([]*domain.DecisionMakerPod{{Host: "dm-host", NodeID: "test", Port: 8080}}, nil).Once()
	suite.MockDMAdapter.EXPECT().DeleteSchedulingIntents(mock.Anything, mock.Anything, mock.Anything).Return(nil).Once()
	suite.deleteIntents(adminToken, []string{intents.Intents[0].ID.Hex()}, http.StatusOK)

	// Verify only one intent remains
	intents = suite.listSelfIntents(adminToken, http.StatusOK)
	suite.Require().Len(intents.Intents, 1, "Expected one intent after deletion")
}

func (suite *HandlerTestSuite) createStrategy(token string, strategyReq *rest.CreateScheduleStrategyRequest, expectedStatus int) {
	createStrategyResp := rest.SuccessResponse[string]{}
	_, resp := suite.sendV1Request("POST", "/strategies", strategyReq, &createStrategyResp, token)
	suite.Require().Equal(expectedStatus, resp.Code, "Unexpected status code on create strategy")
}

func (suite *HandlerTestSuite) listSelfStrategies(token string, expectedStatus int) *rest.ListSchedulerStrategiesResponse {
	listStrategiesResp := rest.SuccessResponse[rest.ListSchedulerStrategiesResponse]{}
	_, resp := suite.sendV1Request("GET", "/strategies/self", nil, &listStrategiesResp, token)
	suite.Require().Equal(expectedStatus, resp.Code, "Unexpected status code on create strategy")
	return listStrategiesResp.Data
}

func (suite *HandlerTestSuite) listSelfIntents(token string, expectedStatus int) *rest.ListScheduleIntentsResponse {
	listStrategiesResp := rest.SuccessResponse[rest.ListScheduleIntentsResponse]{}
	_, resp := suite.sendV1Request("GET", "/intents/self", nil, &listStrategiesResp, token)
	suite.Require().Equal(expectedStatus, resp.Code, "Unexpected status code on create strategy")
	return listStrategiesResp.Data
}

func (suite *HandlerTestSuite) deleteStrategy(token string, strategyID string, expectedStatus int) {
	deleteReq := rest.DeleteScheduleStrategyRequest{
		StrategyID: strategyID,
	}
	deleteResp := rest.SuccessResponse[rest.EmptyResponse]{}
	_, resp := suite.sendV1Request("DELETE", "/strategies", deleteReq, &deleteResp, token)
	suite.Require().Equal(expectedStatus, resp.Code, "Unexpected status code on delete strategy")
}

func (suite *HandlerTestSuite) deleteIntents(token string, intentIDs []string, expectedStatus int) {
	deleteReq := rest.DeleteScheduleIntentsRequest{
		IntentIDs: intentIDs,
	}
	deleteResp := rest.SuccessResponse[rest.EmptyResponse]{}
	_, resp := suite.sendV1Request("DELETE", "/intents", deleteReq, &deleteResp, token)
	suite.Require().Equal(expectedStatus, resp.Code, "Unexpected status code on delete intents")
}
