package admin

import (
	"fmt"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	response "github.com/TokenFlux/TokenRouter/internal/server/httpx"
	"github.com/TokenFlux/TokenRouter/internal/usage"
)

// imported_calls_handler.go serves both administrator views of imported call
// history through one read route. Date-only bounds use the dashboard calendar.
// Detail filters select reported groups and models without resolving local
// request identities or calling the native usage, cleanup, or export routes.

// ListImportedCalls returns additive group counters or a page of imported
// minute rows. Each response carries the source shared by both views.
func (h *UsageHandler) ListImportedCalls(c *gin.Context) {
	view := c.DefaultQuery("view", "groups")
	if view != "groups" && view != "minutes" {
		response.BadRequest(c, "Invalid view, use groups or minutes")
		return
	}

	var groupID int64
	if raw := c.Query("group_id"); raw != "" {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			response.BadRequest(c, "Invalid group_id")
			return
		}
		groupID = id
	}

	start, err := h.importedCallTime(c, "start_date")
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	end, err := h.importedCallTime(c, "end_date")
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if start != nil && end != nil && !end.After(*start) {
		response.BadRequest(c, "end_date must follow start_date")
		return
	}

	page, size := response.ParsePagination(c)
	report, err := h.usageService.ListImportedCalls(c.Request.Context(), usage.ImportedCallQuery{
		Start:       start,
		End:         end,
		GroupID:     groupID,
		Model:       c.Query("model"),
		WithMinutes: view == "minutes",
		Page:        page,
		PageSize:    size,
	})
	if response.ErrorFrom(c, err) {
		return
	}
	response.Success(c, report)
}

// importedCallTime parses an optional bound in the requested calendar.
// An end date without a time includes that local calendar day.
func (h *UsageHandler) importedCallTime(c *gin.Context, key string) (*time.Time, error) {
	raw := c.Query(key)
	if raw == "" {
		return nil, nil
	}
	bound, dateOnly, err := h.calendar.ParseDateTimeInUserLocation(raw, c.Query("timezone"))
	if err != nil {
		return nil, fmt.Errorf("invalid %s", key)
	}
	if key == "end_date" && dateOnly {
		bound = bound.AddDate(0, 0, 1)
	}
	return &bound, nil
}
