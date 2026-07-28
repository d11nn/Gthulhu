package rest

import (
	"net/http"

	"github.com/Gthulhu/api/manager/domain"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type InternalScheduleStrategyResponse struct {
	StrategyID string `json:"strategyId"`
}

// UpsertInternalScheduleStrategy applies controller-owned strategy changes through
// the Manager service so ScheduleIntent reconciliation and DM delivery remain intact.
func (h *Handler) UpsertInternalScheduleStrategy(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req CreateScheduleStrategyRequest
	if err := h.JSONBind(r, &req); err != nil {
		h.ErrorResponse(ctx, w, http.StatusBadRequest, "Invalid request body", err)
		return
	}
	if req.StrategyNamespace == "" {
		h.ErrorResponse(ctx, w, http.StatusBadRequest, "strategyNamespace is required", nil)
		return
	}

	claims, ok := h.GetClaimsFromContext(ctx)
	if !ok {
		h.ErrorResponse(ctx, w, http.StatusUnauthorized, "Unauthorized", nil)
		return
	}
	creatorID, err := claims.GetBsonObjectUID()
	if err != nil {
		h.ErrorResponse(ctx, w, http.StatusUnauthorized, "Invalid internal controller identity", err)
		return
	}

	strategy := &domain.ScheduleStrategy{
		StrategyNamespace: req.StrategyNamespace,
		LabelSelectors:    make([]domain.LabelSelector, len(req.LabelSelectors)),
		K8sNamespace:      req.K8sNamespace,
		CommandRegex:      req.CommandRegex,
		Priority:          req.Priority,
		ExecutionTime:     req.ExecutionTime,
	}
	for i, selector := range req.LabelSelectors {
		strategy.LabelSelectors[i] = domain.LabelSelector{Key: selector.Key, Value: selector.Value}
	}

	query := &domain.QueryStrategyOptions{CreatorIDs: []bson.ObjectID{creatorID}}
	if err := h.Svc.ListScheduleStrategies(ctx, query); err != nil {
		h.HandleError(ctx, w, err)
		return
	}
	for _, existing := range query.Result {
		if existing.StrategyNamespace != req.StrategyNamespace {
			continue
		}
		if err := h.Svc.UpdateScheduleStrategy(ctx, &claims, existing.ID.Hex(), strategy); err != nil {
			h.HandleError(ctx, w, err)
			return
		}
		response := InternalScheduleStrategyResponse{StrategyID: existing.ID.Hex()}
		h.JSONResponse(ctx, w, http.StatusOK, NewSuccessResponse(&response))
		return
	}

	if err := h.Svc.CreateScheduleStrategy(ctx, &claims, strategy); err != nil {
		h.HandleError(ctx, w, err)
		return
	}
	response := InternalScheduleStrategyResponse{StrategyID: strategy.ID.Hex()}
	h.JSONResponse(ctx, w, http.StatusOK, NewSuccessResponse(&response))
}
