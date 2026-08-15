package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/autoseedrelay/relay/internal/store"
	"github.com/gin-gonic/gin"
)

// strategyDTO mirrors store.Strategy on the wire (snake_case). promotions /
// keywords / image_host are JSON columns and pass through as native JSON.
type strategyDTO struct {
	ID                 int64           `json:"id"`
	Promotions         json.RawMessage `json:"promotions"`
	Keywords           json.RawMessage `json:"keywords"`
	MinSize            int64           `json:"min_size"`
	MaxSize            int64           `json:"max_size"`
	RetireSeeders      int64           `json:"retire_seeders"`
	RetireMinutes      int64           `json:"retire_minutes"`
	RetireRatioEnabled bool            `json:"retire_ratio_enabled"`
	RetireRatio        float64         `json:"retire_ratio"`
	RetireMode         string          `json:"retire_mode"`
	DispatchMode       string          `json:"dispatch_mode"`
	Timezone           string          `json:"timezone"`
	ImageHost          json.RawMessage `json:"image_host"`
	ImageCoverEnabled  bool            `json:"image_cover_enabled"`
	RetryMax           int64           `json:"retry_max"`
}

func (d *strategyDTO) toStore() *store.Strategy {
	return &store.Strategy{
		ID:                 1,
		Promotions:         rawToJSONStringDefault(d.Promotions, "[]"),
		Keywords:           rawToJSONStringDefault(d.Keywords, "[]"),
		MinSize:            d.MinSize,
		MaxSize:            d.MaxSize,
		RetireSeeders:      d.RetireSeeders,
		RetireMinutes:      d.RetireMinutes,
		RetireRatioEnabled: boolToInt(d.RetireRatioEnabled),
		RetireRatio:        d.RetireRatio,
		RetireMode:         d.RetireMode,
		DispatchMode:       d.DispatchMode,
		Timezone:           d.Timezone,
		ImageHost:          rawToJSONStringDefault(d.ImageHost, "{}"),
		ImageCoverEnabled:  boolToInt(d.ImageCoverEnabled),
		RetryMax:           d.RetryMax,
	}
}

func strategyDTOFromStore(s *store.Strategy) strategyDTO {
	return strategyDTO{
		ID:                 s.ID,
		Promotions:         jsonStringToRaw(s.Promotions),
		Keywords:           jsonStringToRaw(s.Keywords),
		MinSize:            s.MinSize,
		MaxSize:            s.MaxSize,
		RetireSeeders:      s.RetireSeeders,
		RetireMinutes:      s.RetireMinutes,
		RetireRatioEnabled: s.RetireRatioEnabled != 0,
		RetireRatio:        s.RetireRatio,
		RetireMode:         s.RetireMode,
		DispatchMode:       s.DispatchMode,
		Timezone:           s.Timezone,
		ImageHost:          jsonStringToRaw(s.ImageHost),
		ImageCoverEnabled:  s.ImageCoverEnabled != 0,
		RetryMax:           s.RetryMax,
	}
}

func (h *handler) getStrategy(c *gin.Context) {
	repo := h.repoOr500(c)
	if repo == nil {
		return
	}
	st, err := repo.GetStrategy(c.Request.Context())
	if errors.Is(err, sql.ErrNoRows) {
		writeError(c, http.StatusNotFound, "strategy not found")
		return
	}
	if err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, strategyDTOFromStore(st))
}

func (h *handler) putStrategy(c *gin.Context) {
	repo := h.repoOr500(c)
	if repo == nil {
		return
	}
	var in strategyDTO
	if err := c.ShouldBindJSON(&in); err != nil {
		writeError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	// retire_mode carries a DB CHECK (and|or); validate here for a clean 400.
	if in.RetireMode != "and" && in.RetireMode != "or" {
		writeError(c, http.StatusBadRequest, "invalid retire_mode")
		return
	}
	if err := repo.UpdateStrategy(c.Request.Context(), in.toStore()); err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	st, err := repo.GetStrategy(c.Request.Context())
	if err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, strategyDTOFromStore(st))
}
