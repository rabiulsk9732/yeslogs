package director

import "time"

// istLoc is Asia/Kolkata (IST, UTC+5:30). All timestamps shown/parsed in the
// console are in IST regardless of what an exporter device sends — the platform
// operates in India. Falls back to a fixed +05:30 zone if the tz database is
// unavailable (cmd/natlog imports time/tzdata so this always resolves).
var istLoc = func() *time.Location {
	if l, err := time.LoadLocation("Asia/Kolkata"); err == nil {
		return l
	}
	return time.FixedZone("IST", 5*3600+30*60)
}()
