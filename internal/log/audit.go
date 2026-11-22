package log

import (
	"cmp"
	"os"
	"strconv"

	"github.com/rs/zerolog"
)

// Process names this process as host/pid, as an update claim's owner and an audit record's by.
func Process() string {
	host, _ := os.Hostname()
	return cmp.Or(host, "incus-beacon") + "/" + strconv.Itoa(os.Getpid())
}

// AuditFor adds by, project and instance, leaving out an empty project; README.md#audit-log lists the rest.
func AuditFor(audit zerolog.Logger, by, project, instance string) zerolog.Logger {
	c := audit.With().Str("by", by)
	if project != "" {
		c = c.Str("project", project)
	}
	return c.Str("instance", instance).Logger()
}

// AuditBegin is event's begin record.
func AuditBegin(audit zerolog.Logger, event string) *zerolog.Event {
	return audit.Log().Str("event", event).Str("phase", "begin")
}

// AuditEnd is event's done record, or its failed one with err.
func AuditEnd(audit zerolog.Logger, event string, err error) *zerolog.Event {
	phase := "done"
	if err != nil {
		phase = "failed"
	}
	return audit.Log().Str("event", event).Str("phase", phase).Err(err)
}
