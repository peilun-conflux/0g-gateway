package chain

// This test reproduces, deterministically and offline, the production crash
// captured on the live gateway:
//
//	PANI[1427] Log entry is unavailable yet ip=... zgsNodeSyncHeight=...
//	panic: (*logrus.Entry) ...
//	  logrus.(*Entry).log ... PanicLevel
//	  0g-storage-client/common/util.(*Reminder).remind  reminder.go:52
//	  0g-storage-client/transfer.(*Uploader).waitForLogEntry  uploader.go:757
//	  zgs-gateway/internal/chain.(*Backend).BatchUpload  backend.go:124
//	  zgs-gateway/internal/uploader.(*Worker).Flush/Run
//
// Root cause: the gateway builds the uploader via NewUploaderFromConfig with a
// zero-value UploaderConfig.LogOption. zg_common.NewLogger then runs
// logger.SetLevel(opt.LogLevel) where LogLevel is the unset logrus.Level — and
// logrus.Level's zero value is 0 == PanicLevel. So the uploader's logger sits at
// PanicLevel. When a storage node lags and waitForLogEntry has to wait, the
// SDK's Reminder.Remind logs its "unavailable yet" reminder at logger.Level
// (the else branch), i.e. at PanicLevel — and logrus panics when you log at
// PanicLevel. The worker has no recover(), so the whole gateway dies.
//
// The happy path never hits this: if the node already has the log entry,
// waitForLogEntry returns before Remind is ever called (which is why the live
// SDK tests against a synced node pass).

import (
	"testing"
	"time"

	zg_common "github.com/0gfoundation/0g-storage-client/common"
	"github.com/0gfoundation/0g-storage-client/common/util"
	"github.com/sirupsen/logrus"
)

// TestReminderPanicsAtZeroLogOption proves the exact trigger: a logger built
// from a zero-value LogOption is at PanicLevel, and Reminder.Remind (the
// sub-interval else branch the SDK takes while polling) panics on it.
func TestReminderPanicsAtZeroLogOption(t *testing.T) {
	// This is exactly what NewUploaderFromConfig(... UploaderConfig{Nodes: ...})
	// does: one LogOption arg, zero-valued.
	logger := zg_common.NewLogger(zg_common.LogOption{})
	if logger.Level != logrus.PanicLevel {
		t.Fatalf("expected uploader logger at PanicLevel(0), got %v(%d)", logger.Level, logger.Level)
	}

	// Reproduce the waitForLogEntry call site: a 1-minute reminder, reminded
	// immediately (within the interval -> else branch -> logs at logger.Level).
	reminder := util.NewReminder(logger, time.Minute)

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected Reminder.Remind to panic at PanicLevel, but it did not")
		}
		t.Logf("reproduced the production panic: %T %v", r, r)
	}()
	reminder.RemindWith("Log entry is unavailable yet", "zgsNodeSyncHeight", 254890094)
	t.Fatal("unreachable: Remind should have panicked")
}

// TestReminderSafeAtErrorLevel proves the fix: building the logger with any
// level >= ErrorLevel (here we cover Error/Warn/Info) makes the same
// sub-interval Remind a no-op-severity log instead of a panic.
func TestReminderSafeAtErrorLevel(t *testing.T) {
	for _, lvl := range []logrus.Level{logrus.ErrorLevel, logrus.WarnLevel, logrus.InfoLevel} {
		logger := zg_common.NewLogger(zg_common.LogOption{LogLevel: lvl})
		if logger.Level != lvl {
			t.Fatalf("expected logger at %v, got %v", lvl, logger.Level)
		}
		reminder := util.NewReminder(logger, time.Minute)
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("level %v: Remind must not panic, but did: %v", lvl, r)
				}
			}()
			reminder.RemindWith("Log entry is unavailable yet", "zgsNodeSyncHeight", 254890094)
		}()
	}
}
