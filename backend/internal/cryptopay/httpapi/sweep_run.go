package httpapi

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/TokenFlux/TokenRouter/internal/server/httpx"
)

// sweep_run.go provides the polling endpoints for asynchronous sweep runs.
// The sweep service holds the current run snapshots and returns recent entries
// in display order. A request for a missing run ID returns a not-found response
// so the console can end polling.

// AdminSweepRunStatus reports one asynchronous sweep run.
// GET /api/v1/admin/crypto/sweep/runs/:id
func (h *CryptoHandler) AdminSweepRunStatus(c *gin.Context) {
	if h.service == nil {
		httpx.BadRequest(c, "crypto channel unavailable")
		return
	}
	run := h.service.SweepRun(c.Param("id"))
	if run == nil {
		httpx.Error(c, http.StatusNotFound, "sweep run not found")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": run})
}

// AdminSweepRuns lists recent asynchronous runs, newest first.
// GET /api/v1/admin/crypto/sweep/runs
func (h *CryptoHandler) AdminSweepRuns(c *gin.Context) {
	if h.service == nil {
		httpx.BadRequest(c, "crypto channel unavailable")
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": h.service.SweepRuns()})
}
