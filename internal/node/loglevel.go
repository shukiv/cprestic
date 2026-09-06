package node

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/shukiv/gniza/internal/nodestore"
)

// LogLevel is the level the running service is logging at. It is a
// slog.LevelVar rather than a number so that changing it changes what the
// loggers already handed out will write, without a restart: the reason to
// turn debug on is almost always something happening now, which a restart
// would end.
//
// A server built without one gets its own, so nothing has to check for nil.
func (e *Engine) LogLevel() *slog.LevelVar { return e.logLevel }

// SetLogLevel moves the running service to a level and records it, so a
// restart comes back at the same one. An unreadable name is refused and
// leaves the service where it was: a typo must not turn the log off.
func (e *Engine) SetLogLevel(name string) error {
	level, err := ParseLogLevel(name)
	if err != nil {
		return err
	}
	settings, err := e.store.Settings()
	if err != nil {
		return err
	}
	settings.LogLevel = strings.ToLower(strings.TrimSpace(name))
	if err := e.store.SaveSettings(settings); err != nil {
		return err
	}
	e.logLevel.Set(level)
	return nil
}

// ApplyStoredLogLevel puts the service at the stored level. It is called at
// startup, where a stored level that cannot be read is not worth refusing
// to start over -- the service logs at info and says so.
func (e *Engine) ApplyStoredLogLevel() {
	settings, err := e.store.Settings()
	if err != nil {
		return
	}
	if settings.LogLevel == "" {
		return
	}
	level, err := ParseLogLevel(settings.LogLevel)
	if err != nil {
		e.log.Warn("the stored log level is not one this service knows",
			"level", settings.LogLevel, "using", nodestore.DefaultLogLevel)
		return
	}
	e.logLevel.Set(level)
}

// ParseLogLevel reads one of nodestore.LogLevels. slog does the reading, so
// the setting, the -log-level flag and the level in every written line all
// understand the same spellings.
func ParseLogLevel(name string) (slog.Level, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(strings.TrimSpace(name))); err != nil {
		return level, fmt.Errorf("a log level must be one of %s", strings.Join(nodestore.LogLevels, ", "))
	}
	return level, nil
}
