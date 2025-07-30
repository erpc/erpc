package erpc

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/erpc/erpc/auth"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
)

// API Key structure for management
type ApiKey struct {
	Key                string    `json:"key"`
	UserId             string    `json:"userId"`
	PerSecondRateLimit *int64    `json:"perSecondRateLimit,omitempty"`
	Enabled            bool      `json:"enabled"`
	CreatedAt          time.Time `json:"createdAt"`
	UpdatedAt          time.Time `json:"updatedAt"`
}

func (e *ERPC) AdminAuthenticate(ctx context.Context, method string, ap *auth.AuthPayload) (*common.User, error) {
	if e.adminAuthRegistry != nil {
		return e.adminAuthRegistry.Authenticate(ctx, method, ap)
	}
	return nil, fmt.Errorf("admin auth not configured")
}

func (e *ERPC) AdminHandleRequest(ctx context.Context, nq *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	method, err := nq.Method()
	if err != nil {
		return nil, err
	}

	switch method {
	case "erpc_taxonomy":
		return e.handleTaxonomy(ctx, nq)
	case "erpc_config":
		return e.handleConfig(ctx, nq)
	case "erpc_project":
		return e.handleProject(ctx, nq)
	case "erpc_addApiKey":
		return e.handleAddApiKey(ctx, nq)
	case "erpc_listApiKeys":
		return e.handleListApiKeys(ctx, nq)
	case "erpc_updateApiKey":
		return e.handleUpdateApiKey(ctx, nq)
	case "erpc_deleteApiKey":
		return e.handleDeleteApiKey(ctx, nq)

	default:
		return nil, common.NewErrEndpointUnsupported(
			fmt.Errorf("admin method %s is not supported", method),
		)
	}
}

// findDatabaseConnectorById finds a database connector by ID from admin auth strategies
func (e *ERPC) findDatabaseConnectorById(projectId, connectorId string) (data.Connector, error) {
	if e.projectsRegistry == nil {
		return nil, fmt.Errorf("projects registry not configured")
	}

	// Get the prepared project
	preparedProject := e.projectsRegistry.preparedProjects[projectId]
	if preparedProject == nil {
		return nil, fmt.Errorf("project '%s' not found", projectId)
	}

	// Get the project's auth registry
	if preparedProject.consumerAuthRegistry == nil {
		return nil, fmt.Errorf("project '%s' has no auth registry", projectId)
	}

	// Find the database connector within the project
	return preparedProject.consumerAuthRegistry.FindDatabaseConnector(connectorId)
}

// handleAddApiKey adds a new API key
func (e *ERPC) handleAddApiKey(ctx context.Context, nq *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	jrr, err := nq.JsonRpcRequest()
	if err != nil {
		return nil, err
	}

	if len(jrr.Params) < 1 {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("requires params: {projectId, connectorId, apiKey, userId, perSecondRateLimit?}"))
	}

	params, ok := jrr.Params[0].(map[string]interface{})
	if !ok {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("first parameter must be an object"))
	}

	projectId, ok := params["projectId"].(string)
	if !ok || projectId == "" {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("projectId is required and must be a string"))
	}

	connectorId, ok := params["connectorId"].(string)
	if !ok || connectorId == "" {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("connectorId is required and must be a string"))
	}

	apiKey, ok := params["apiKey"].(string)
	if !ok || apiKey == "" {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("apiKey is required and must be a string"))
	}

	userId, ok := params["userId"].(string)
	if !ok || userId == "" {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("userId is required and must be a string"))
	}

	var perSecondRateLimit *int64
	if rateLimit, exists := params["perSecondRateLimit"]; exists && rateLimit != nil {
		if rateLimitFloat, ok := rateLimit.(float64); ok {
			rateLimitInt := int64(rateLimitFloat)
			perSecondRateLimit = &rateLimitInt
		} else {
			return nil, common.NewErrInvalidRequest(fmt.Errorf("perSecondRateLimit must be a number"))
		}
	}

	connector, err := e.findDatabaseConnectorById(projectId, connectorId)
	if err != nil {
		return nil, fmt.Errorf("failed to find connector: %w", err)
	}

	// Create user data
	userData := map[string]interface{}{
		"userId":  userId,
		"enabled": true, // New API keys are enabled by default
	}
	if perSecondRateLimit != nil {
		userData["perSecondRateLimit"] = *perSecondRateLimit
	}

	userDataBytes, err := json.Marshal(userData)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal user data: %w", err)
	}

	err = connector.Set(ctx, apiKey, userId, userDataBytes, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to store API key: %w", err)
	}

	result := map[string]interface{}{
		"success": true,
		"apiKey":  apiKey,
		"userId":  userId,
	}

	jrrs, err := common.NewJsonRpcResponse(jrr.ID, result, nil)
	if err != nil {
		return nil, err
	}

	return common.NewNormalizedResponse().WithJsonRpcResponse(jrrs), nil
}

// handleListApiKeys lists API keys for a connector
func (e *ERPC) handleListApiKeys(ctx context.Context, nq *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	jrr, err := nq.JsonRpcRequest()
	if err != nil {
		return nil, err
	}

	if len(jrr.Params) < 1 {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("requires params: {projectId, connectorId, limit?, paginationToken?}"))
	}

	params, ok := jrr.Params[0].(map[string]interface{})
	if !ok {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("first parameter must be an object"))
	}

	projectId, ok := params["projectId"].(string)
	if !ok || projectId == "" {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("projectId is required and must be a string"))
	}

	connectorId, ok := params["connectorId"].(string)
	if !ok || connectorId == "" {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("connectorId is required and must be a string"))
	}

	limit := 50 // default
	if limitVal, exists := params["limit"]; exists && limitVal != nil {
		if limitFloat, ok := limitVal.(float64); ok {
			limit = int(limitFloat)
		}
	}

	paginationToken := ""
	if tokenVal, exists := params["paginationToken"]; exists && tokenVal != nil {
		if tokenStr, ok := tokenVal.(string); ok {
			paginationToken = tokenStr
		}
	}

	connector, err := e.findDatabaseConnectorById(projectId, connectorId)
	if err != nil {
		return nil, fmt.Errorf("failed to find connector: %w", err)
	}

	// List from main index to get all API keys (optimized: only 1 row per API key)
	results, nextToken, err := connector.List(ctx, data.ConnectorMainIndex, limit, paginationToken)
	if err != nil {
		return nil, fmt.Errorf("failed to list API keys: %w", err)
	}

	apiKeys := make([]ApiKey, 0)
	for _, item := range results {
		// Parse user data from the value
		var userData map[string]interface{}
		if err := json.Unmarshal(item.Value, &userData); err != nil {
			continue // Skip invalid records
		}

		// Create API key object
		apiKey := ApiKey{
			Key:       item.PartitionKey,
			UserId:    item.RangeKey, // Range key is now the userId directly
			Enabled:   true,          // Default to enabled if not specified
			CreatedAt: time.Now(),    // TODO: Add actual timestamps
			UpdatedAt: time.Now(),
		}

		// Read the enabled status from the database
		if enabled, ok := userData["enabled"]; ok {
			if enabledBool, ok := enabled.(bool); ok {
				apiKey.Enabled = enabledBool
			}
		}

		if rate, ok := userData["perSecondRateLimit"]; ok {
			if rateFloat, ok := rate.(float64); ok {
				rateInt := int64(rateFloat)
				apiKey.PerSecondRateLimit = &rateInt
			}
		}

		apiKeys = append(apiKeys, apiKey)
	}

	result := map[string]interface{}{
		"apiKeys":       apiKeys,
		"nextToken":     nextToken,
		"hasMore":       nextToken != "",
		"totalReturned": len(apiKeys),
	}

	jrrs, err := common.NewJsonRpcResponse(jrr.ID, result, nil)
	if err != nil {
		return nil, err
	}

	return common.NewNormalizedResponse().WithJsonRpcResponse(jrrs), nil
}

// handleUpdateApiKey updates an existing API key
func (e *ERPC) handleUpdateApiKey(ctx context.Context, nq *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	jrr, err := nq.JsonRpcRequest()
	if err != nil {
		return nil, err
	}

	if len(jrr.Params) < 1 {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("requires params: {projectId, connectorId, apiKey, updates}"))
	}

	params, ok := jrr.Params[0].(map[string]interface{})
	if !ok {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("first parameter must be an object"))
	}

	projectId, ok := params["projectId"].(string)
	if !ok || projectId == "" {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("projectId is required and must be a string"))
	}

	connectorId, ok := params["connectorId"].(string)
	if !ok || connectorId == "" {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("connectorId is required and must be a string"))
	}

	apiKey, ok := params["apiKey"].(string)
	if !ok || apiKey == "" {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("apiKey is required and must be a string"))
	}

	updates, ok := params["updates"].(map[string]interface{})
	if !ok {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("updates is required and must be an object"))
	}

	connector, err := e.findDatabaseConnectorById(projectId, connectorId)
	if err != nil {
		return nil, fmt.Errorf("failed to find connector: %w", err)
	}

	currentBytes, err := connector.Get(ctx, data.ConnectorMainIndex, apiKey, "*")
	if err != nil {
		return nil, fmt.Errorf("failed to get current API key data: %w", err)
	}

	var currentData map[string]interface{}
	if err := json.Unmarshal(currentBytes, &currentData); err != nil {
		return nil, fmt.Errorf("failed to parse current data: %w", err)
	}

	// Get the userId from current data to know the range key
	userId, ok := currentData["userId"].(string)
	if !ok || userId == "" {
		return nil, fmt.Errorf("missing or invalid userId in current data")
	}

	// Apply updates
	for key, value := range updates {
		currentData[key] = value
	}

	// Save updated data to the same location
	updatedBytes, err := json.Marshal(currentData)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal updated data: %w", err)
	}

	err = connector.Set(ctx, apiKey, userId, updatedBytes, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to update API key: %w", err)
	}

	result := map[string]interface{}{
		"success": true,
		"apiKey":  apiKey,
		"updated": updates,
	}

	jrrs, err := common.NewJsonRpcResponse(jrr.ID, result, nil)
	if err != nil {
		return nil, err
	}

	return common.NewNormalizedResponse().WithJsonRpcResponse(jrrs), nil
}

// handleDeleteApiKey deletes an API key and its reverse index
func (e *ERPC) handleDeleteApiKey(ctx context.Context, nq *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	jrr, err := nq.JsonRpcRequest()
	if err != nil {
		return nil, err
	}

	if len(jrr.Params) < 1 {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("requires params: {projectId, connectorId, apiKey}"))
	}

	params, ok := jrr.Params[0].(map[string]interface{})
	if !ok {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("first parameter must be an object"))
	}

	projectId, ok := params["projectId"].(string)
	if !ok || projectId == "" {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("projectId is required and must be a string"))
	}

	connectorId, ok := params["connectorId"].(string)
	if !ok || connectorId == "" {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("connectorId is required and must be a string"))
	}

	apiKey, ok := params["apiKey"].(string)
	if !ok || apiKey == "" {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("apiKey is required and must be a string"))
	}

	connector, err := e.findDatabaseConnectorById(projectId, connectorId)
	if err != nil {
		return nil, fmt.Errorf("failed to find connector: %w", err)
	}

	// Get current data to find the userId (range key) for deletion
	currentBytes, err := connector.Get(ctx, data.ConnectorMainIndex, apiKey, "*")
	if err != nil {
		return nil, fmt.Errorf("failed to get current API key data: %w", err)
	}

	var currentData map[string]interface{}
	if err := json.Unmarshal(currentBytes, &currentData); err != nil {
		return nil, fmt.Errorf("failed to parse current data: %w", err)
	}

	// Get the userId from current data to know the range key
	userId, ok := currentData["userId"].(string)
	if !ok || userId == "" {
		return nil, fmt.Errorf("missing or invalid userId in current data")
	}

	err = connector.Delete(ctx, apiKey, userId)
	if err != nil {
		return nil, fmt.Errorf("failed to delete API key: %w", err)
	}

	result := map[string]interface{}{
		"success": true,
		"apiKey":  apiKey,
		"userId":  userId,
	}

	jrrs, err := common.NewJsonRpcResponse(jrr.ID, result, nil)
	if err != nil {
		return nil, err
	}

	return common.NewNormalizedResponse().WithJsonRpcResponse(jrrs), nil
}

// handleConfig returns the eRPC configuration
func (e *ERPC) handleConfig(ctx context.Context, nq *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	jrr, err := nq.JsonRpcRequest()
	if err != nil {
		return nil, err
	}

	jrrs, err := common.NewJsonRpcResponse(
		jrr.ID,
		e.cfg,
		nil,
	)
	if err != nil {
		return nil, err
	}
	return common.NewNormalizedResponse().WithJsonRpcResponse(jrrs), nil
}

// handleTaxonomy returns the taxonomy of projects, networks, and upstreams
func (e *ERPC) handleTaxonomy(ctx context.Context, nq *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	jrr, err := nq.JsonRpcRequest()
	if err != nil {
		return nil, err
	}

	type taxonomyUpstream struct {
		Id     string `json:"id"`
		Vendor string `json:"vendor"`
	}
	type taxonomyProvider struct {
		Id     string `json:"id"`
		Vendor string `json:"vendor"`
	}
	type taxonomyNetwork struct {
		Id        string              `json:"id"`
		Alias     string              `json:"alias"`
		Upstreams []*taxonomyUpstream `json:"upstreams"`
		Providers []*taxonomyProvider `json:"providers"`
	}
	type taxonomyProject struct {
		Id       string             `json:"id"`
		Networks []*taxonomyNetwork `json:"networks"`
	}
	type taxonomyResult struct {
		Projects []*taxonomyProject `json:"projects"`
	}

	result := &taxonomyResult{}
	projects := e.GetProjects()
	for _, p := range projects {
		networks := []*taxonomyNetwork{}
		for _, n := range p.GetNetworks() {
			ntw := &taxonomyNetwork{
				Id:        n.Id(),
				Upstreams: []*taxonomyUpstream{},
			}
			if n.cfg != nil && n.cfg.Alias != "" {
				ntw.Alias = n.cfg.Alias
			}
			upstreams := n.upstreamsRegistry.GetNetworkUpstreams(ctx, n.Id())
			for _, u := range upstreams {
				ups := taxonomyUpstream{
					Id: u.Id(),
				}
				if u.Vendor() != nil {
					ups.Vendor = u.Vendor().Name()
				}
				ntw.Upstreams = append(ntw.Upstreams, &ups)
			}
			networks = append(networks, ntw)
		}
		result.Projects = append(result.Projects, &taxonomyProject{
			Id:       p.Config.Id,
			Networks: networks,
		})
	}

	jrrs, err := common.NewJsonRpcResponse(
		jrr.ID,
		result,
		nil,
	)
	if err != nil {
		return nil, err
	}
	return common.NewNormalizedResponse().WithJsonRpcResponse(jrrs), nil
}

// handleProject returns the configuration and health information for a specific project
func (e *ERPC) handleProject(ctx context.Context, nq *common.NormalizedRequest) (*common.NormalizedResponse, error) {
	jrr, err := nq.JsonRpcRequest()
	if err != nil {
		return nil, err
	}

	type configResult struct {
		Config *common.ProjectConfig `json:"config"`
		Health *ProjectHealthInfo    `json:"health"`
	}

	if len(jrr.Params) == 0 {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("project id (params[0]) is required"))
	}

	pid, ok := jrr.Params[0].(string)
	if !ok {
		return nil, common.NewErrInvalidRequest(fmt.Errorf("project id (params[0]) must be a string"))
	}

	p, err := e.GetProject(pid)
	if err != nil {
		return nil, err
	}

	health, err := p.GatherHealthInfo()
	if err != nil {
		return nil, err
	}

	result := configResult{
		Config: p.Config,
		Health: health,
	}

	jrrs, err := common.NewJsonRpcResponse(
		jrr.ID,
		result,
		nil,
	)
	if err != nil {
		return nil, err
	}
	return common.NewNormalizedResponse().WithJsonRpcResponse(jrrs), nil
}
