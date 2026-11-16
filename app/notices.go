package app

import (
	"fmt"
	"time"
)

// noticeAfter is how long a destination must have refused an entry before
// the admin area says so.
var noticeAfter = time.Minute

// adminNotices is what every page of the admin area tells its admins:
// saved changes that are not reaching where they go. A destination that
// keeps refusing an entry of the change log (a database whose table will
// not take a row the model allows) holds the entry, and everything saved
// after it, in the log: nothing is lost, but none of it is in the
// destination, and the pages — drawn from what the destination holds once
// the server starts again — would then be without it. That was once seen
// only in the server's log; an admin is told here, as soon as it has gone
// on for a minute.
func (a *App) adminNotices() []string {
	var out []string
	for _, s := range a.Reg.StuckEntries() {
		if time.Since(s.Since) < noticeAfter {
			continue // a moment's trouble mends itself
		}
		behind := ""
		switch {
		case s.Waiting == 1:
			behind = " One later change is waiting behind it."
		case s.Waiting > 1:
			behind = fmt.Sprintf(" %d later changes are waiting behind it.", s.Waiting)
		}
		out = append(out, fmt.Sprintf("Saved changes are not reaching “%s”. For %s it has refused a change (%s): %s.%s Nothing is lost — they are kept in the change log and are applied once this is put right — but until then they are not stored there, and would go missing from the pages if the server restarted. Tell whoever runs this server.",
			s.Listener, humanDuration(time.Since(s.Since).Round(time.Second)), s.Changes, s.Error, behind))
	}
	if n := len(a.Reg.ParkedEntries()); n > 0 {
		out = append(out, fmt.Sprintf("%d change(s) could not be delivered to a service and were set aside (parked). Whoever runs this server can replay or drop them.", n))
	}
	return out
}
