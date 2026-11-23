package admin

import "github.com/voidmind-io/voidllm/internal/db"

// TeamUpdateRequest mirrors the unexported updateTeamRequest so the admin_test
// package can build one for TeamUpdateParams.
type TeamUpdateRequest struct {
	Name              *string
	DailyTokenLimit   *int64
	MonthlyTokenLimit *int64
	RequestsPerMinute *int
	RequestsPerDay    *int
}

// TeamUpdateParams exposes the internal teamUpdateParams function for
// white-box testing from the admin_test package: the mapping from a PATCH
// request and the caller's role to the fields UpdateTeam writes.
func TeamUpdateParams(callerRole string, current *db.Team, req TeamUpdateRequest) (db.UpdateTeamParams, bool) {
	return teamUpdateParams(callerRole, current, &updateTeamRequest{
		Name:              req.Name,
		DailyTokenLimit:   req.DailyTokenLimit,
		MonthlyTokenLimit: req.MonthlyTokenLimit,
		RequestsPerMinute: req.RequestsPerMinute,
		RequestsPerDay:    req.RequestsPerDay,
	})
}
